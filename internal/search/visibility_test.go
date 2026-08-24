package search

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// visibility_test.go covers the mandatory visibility clause: the part of
// per-page privacy that has to happen inside the bleve query, before
// ranking and before truncation, because no amount of filtering
// afterwards can undo either.

// privatePage is a page private to owner, in the reserved page-ID space
// a personal memory occupies.
func privatePage(owner, slug, title, body string) kb.Page {
	return fixturePage(kb.PrivatePrefix+owner+"/"+slug, title, body, "memory")
}

func TestQueryAs_HidesAnotherOwnersPrivatePage(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{
		fixturePage("handbook/onboarding", "Onboarding", "the aardvark is public knowledge", "handbook"),
		privatePage("alice-1111111111111111", "note", "Alice note", "the aardvark is alice's secret"),
	})

	alice := kb.AsOwner("alice-1111111111111111")
	bob := kb.AsOwner("bob-2222222222222222")

	for _, tc := range []struct {
		name   string
		viewer kb.Viewer
		want   int
	}{
		{"the owner sees both", alice, 2},
		{"another principal sees only the public page", bob, 1},
		{"a caller who owns nothing sees only the public page", kb.AsOwner(""), 1},
		{"an unfiltered viewer sees both", kb.Unfiltered(), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := idx.QueryAs(context.Background(), tc.viewer, "aardvark", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				ids := make([]string, len(got))
				for i, r := range got {
					ids[i] = r.Page.ID
				}
				t.Fatalf("got %d hits %v, want %d", len(got), ids, tc.want)
			}
			for _, r := range got {
				if o := r.Page.PrivateOwner(); o != "" && !tc.viewer.CanSeeOwner(o) {
					t.Errorf("hit %q is private to %q", r.Page.ID, o)
				}
				// A snippet is metadata too: a filtered-out document must
				// not contribute one, and the ones that survive belong to
				// this viewer.
				if strings.Contains(r.Snippet, "alice's secret") && !tc.viewer.CanSeeOwner("alice-1111111111111111") {
					t.Errorf("snippet leaks a private body: %q", r.Snippet)
				}
			}
		})
	}
}

// TestQueryAs_FiltersBeforeTruncation is the test the whole design turns
// on. Fifty private documents outrank the one public document a
// different caller may see; with a limit of five, a post-filter over the
// top five hits would return NOTHING, and the caller would be told —
// truthfully, from its point of view — that the knowledge base has no
// answer. The clause in the query means bleve never considers the
// private documents at all, so the five best VISIBLE documents come
// back.
func TestQueryAs_FiltersBeforeRankingAndTruncation(t *testing.T) {
	pages := []kb.Page{
		// One public page, deliberately the weakest match: the term
		// appears once, in the body only.
		fixturePage("handbook/onboarding", "Onboarding", "an occasional mention of capybara here", "handbook"),
	}
	for i := range 50 {
		// Private pages with the term in the title AND the ID, which is
		// boosted 5x and 3x — every one of them outranks the public page.
		pages = append(pages, privatePage(
			"alice-1111111111111111",
			fmt.Sprintf("capybara-%02d", i),
			fmt.Sprintf("Capybara %02d", i),
			"capybara capybara capybara",
		))
	}
	idx := newTestIndex(t, pages)

	got, err := idx.QueryAs(context.Background(), kb.AsOwner("bob-2222222222222222"), "capybara", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d hits, want the 1 visible one — the limit was consumed by hidden documents", len(got))
	}
	if got[0].Page.ID != "handbook/onboarding" {
		t.Errorf("hit = %q, want handbook/onboarding", got[0].Page.ID)
	}

	// The owner, asking the same question with the same limit, gets a
	// full page of results — so the filter is not simply suppressing
	// everything.
	own, err := idx.QueryAs(context.Background(), kb.AsOwner("alice-1111111111111111"), "capybara", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 5 {
		t.Errorf("the owner got %d hits, want 5", len(own))
	}
}

// TestQueryAs_DoesNotChangeScores pins that visibility decides
// eligibility and not ranking: the clause is boosted to zero, so a
// restricted caller's scores for the documents they CAN see are the
// scores an unrestricted caller gets for the same documents.
func TestQueryAs_DoesNotChangeScores(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{
		fixturePage("concepts/eviction", "Cache eviction", "the cache evicts entries on pressure", "concepts"),
		fixturePage("runbooks/cache", "Cache runbook", "restart the cache, then evict", "runbooks"),
		fixturePage("handbook/glossary", "Glossary", "cache: a small fast store", "handbook"),
	})

	unfiltered, err := idx.QueryAs(context.Background(), kb.Unfiltered(), "cache", 10)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := idx.QueryAs(context.Background(), kb.AsOwner("alice-1111111111111111"), "cache", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered) != len(filtered) || len(filtered) == 0 {
		t.Fatalf("hit counts differ: %d unfiltered, %d filtered", len(unfiltered), len(filtered))
	}
	for i := range unfiltered {
		if unfiltered[i].Page.ID != filtered[i].Page.ID {
			t.Fatalf("hit %d: order differs — %q unfiltered, %q filtered",
				i, unfiltered[i].Page.ID, filtered[i].Page.ID)
		}
		if unfiltered[i].Score != filtered[i].Score {
			t.Errorf("hit %d (%s): score %v filtered, %v unfiltered — the visibility clause is contributing to the score",
				i, filtered[i].Page.ID, filtered[i].Score, unfiltered[i].Score)
		}
	}
}

// TestQueryAs_OwnerTokenIsNotFreeTextSearchable pins that the visibility
// field is not reachable from a free-text query: a caller must not be
// able to search for another principal's owner token and learn from the
// result count that the principal exists.
func TestQueryAs_OwnerTokenIsNotFreeTextSearchable(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{
		fixturePage("handbook/onboarding", "Onboarding", "public text", "handbook"),
		privatePage("alice-1111111111111111", "note", "Note", "private text"),
	})
	bob := kb.AsOwner("bob-2222222222222222")

	for _, q := range []string{
		"own:alice-1111111111111111",
		`owner:"own:alice-1111111111111111"`,
		"alice-1111111111111111",
		"public",
	} {
		got, err := idx.QueryAs(context.Background(), bob, q, 10)
		if err != nil {
			// A query the parser rejects is fine; a query that returns
			// somebody else's document is not.
			continue
		}
		for _, r := range got {
			if r.Page.PrivateOwner() != "" {
				t.Errorf("query %q surfaced private page %q to another principal", q, r.Page.ID)
			}
		}
	}
}

// TestPut_IndexesVisibilityForALivePage pins that a memory saved into a
// LIVE index carries its visibility immediately — the incremental path
// and the bulk build share indexDoc precisely so they cannot disagree.
func TestPut_IndexesVisibilityForALivePage(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{
		fixturePage("handbook/onboarding", "Onboarding", "unrelated", "handbook"),
	})
	page := privatePage("alice-1111111111111111", "wombat", "Wombat", "the wombat detail")
	if err := idx.Put(page); err != nil {
		t.Fatalf("Put: %v", err)
	}

	own, err := idx.QueryAs(context.Background(), kb.AsOwner("alice-1111111111111111"), "wombat", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 {
		t.Errorf("the owner got %d hits for their own just-saved memory, want 1", len(own))
	}
	other, err := idx.QueryAs(context.Background(), kb.AsOwner("bob-2222222222222222"), "wombat", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("another principal got %d hits for a private memory, want 0", len(other))
	}
}

// TestQueryAs_IsSafeAlongsideConcurrentPuts runs restricted queries and
// incremental writes against one index at once. The registry shares a
// single *Index across every MCP session, so this is the real shape: one
// principal saving while others search, each seeing only their own.
func TestQueryAs_IsSafeAlongsideConcurrentPuts(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{
		fixturePage("handbook/onboarding", "Onboarding", "shared body text", "handbook"),
	})
	owners := []string{"alice-1111111111111111", "bob-2222222222222222", "carol-333333333333333"}

	var wg sync.WaitGroup
	for i, owner := range owners {
		wg.Add(1)
		go func(i int, owner string) {
			defer wg.Done()
			for j := range 10 {
				p := privatePage(owner, fmt.Sprintf("n-%d-%d", i, j), "Note", "shared body text")
				if err := idx.Put(p); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
			}
		}(i, owner)
	}
	for _, owner := range owners {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			v := kb.AsOwner(owner)
			for range 20 {
				got, err := idx.QueryAs(context.Background(), v, "shared body", 200)
				if err != nil {
					t.Errorf("QueryAs: %v", err)
					return
				}
				for _, r := range got {
					if o := r.Page.PrivateOwner(); o != "" && o != owner {
						t.Errorf("viewer %q saw %q owned by %q", owner, r.Page.ID, o)
						return
					}
				}
			}
		}(owner)
	}
	wg.Wait()

	// Each owner ends up with their own ten, plus the one public page.
	for _, owner := range owners {
		got, err := idx.QueryAs(context.Background(), kb.AsOwner(owner), "shared body", 200)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 11 {
			t.Errorf("owner %q sees %d pages, want 11 (10 own + 1 public)", owner, len(got))
		}
	}
	all, err := idx.QueryAs(context.Background(), kb.Unfiltered(), "shared body", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 31 {
		t.Errorf("unfiltered sees %d pages, want 31", len(all))
	}
}
