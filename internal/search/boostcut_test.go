package search

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// boostcut_test.go pins issue #87: type boosts are applied BEFORE the
// result set is cut to the limit, so the answer does not depend on how
// many results were asked for.

// TestTypeBoosts_AppliedBeforeTheCut is the #87 reproduction shape: a
// pointer whose RAW score is below a content page's, but whose ×4
// boosted score wins. Asking for one result must return the pointer —
// the same top hit a request for ten returns.
func TestTypeBoosts_AppliedBeforeTheCut(t *testing.T) {
	pages := []kb.Page{
		fixturePage("concepts/error-budget-policy", "Error budget policy", "What a team does when the error budget is spent.", "concepts"),
		pointerAt("pointers/sre-workbook/error-budget-policy", "Example error budget policy", "Chapter pointer."),
		fixturePage("concepts/error-budget", "Error budget", "One minus the SLO, spent by unreliability.", "concepts"),
	}
	const q = "team gaming the error budget policy by staying red"

	// Precondition, so the test cannot pass vacuously: unboosted, the
	// pointer is NOT the top raw hit.
	raw := newTestIndex(t, pages, WithTypeBoosts(map[string]float64{}))
	rawRes, err := raw.Query(q, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rawRes) < 2 || rawRes[0].Page.ID == "pointers/sre-workbook/error-budget-policy" {
		t.Fatalf("fixture: the pointer must lose on raw score, got %v", ids(rawRes))
	}

	idx := newTestIndex(t, pages) // DefaultTypeBoosts: pointer ×4
	ten, err := idx.Query(q, 10)
	if err != nil {
		t.Fatal(err)
	}
	if ten[0].Page.ID != "pointers/sre-workbook/error-budget-policy" {
		t.Fatalf("fixture: with the ×4 the pointer must win at limit 10, got %v", ids(ten))
	}
	for n := 1; n <= len(ten); n++ {
		got, err := idx.Query(q, n)
		if err != nil {
			t.Fatal(err)
		}
		if want := ids(ten[:n]); !slices.Equal(ids(got), want) {
			t.Errorf("limit %d = %v, want %v (the first %d of limit 10)", n, ids(got), want, n)
		}
	}
}

// TestTypeBoosts_ResultsArePrefixStable is #87's acceptance criterion
// over a generated corpus: for every query, the first n results of a
// larger limit equal the results of limit n — at the exact, fuzzy and
// prefix stages, for an unfiltered and a restricted viewer, under the
// default boosts and under weights that DEMOTE a type (below 1), which
// reorders in the other direction.
func TestTypeBoosts_ResultsArePrefixStable(t *testing.T) {
	pages := boostCorpus()
	queries := []struct {
		q     string
		stage Stage
	}{
		{"rollback canary", StageExact},
		{"incident latency budget", StageExact},
		{"quorum", StageExact},
		{"rolback", StageFuzzy},
		{"rollb", StagePrefix},
	}
	viewers := map[string]kb.Viewer{
		"unfiltered": kb.Unfiltered(),
		"restricted": kb.AsOwner("bob-2222222222222222"),
	}
	for boostsName, opts := range map[string][]Option{
		"defaults": nil,
		"demoting": {WithTypeBoosts(map[string]float64{kb.TypePointer: 3, kb.TypeSkill: 0.25})},
	} {
		idx := newTestIndex(t, pages, opts...)
		for viewerName, v := range viewers {
			for _, tc := range queries {
				t.Run(fmt.Sprintf("%s/%s/%s", boostsName, viewerName, tc.q), func(t *testing.T) {
					const n = 20
					all, stage, err := idx.QueryStaged(context.Background(), v, tc.q, n)
					if err != nil {
						t.Fatal(err)
					}
					if stage != tc.stage {
						t.Fatalf("stage = %s, want %s", stage, tc.stage)
					}
					if len(all) < 5 {
						t.Fatalf("only %d hits; the corpus should give this query more", len(all))
					}
					for k := 1; k <= len(all); k++ {
						got, _, err := idx.QueryStaged(context.Background(), v, tc.q, k)
						if err != nil {
							t.Fatal(err)
						}
						if want := ids(all[:k]); !slices.Equal(ids(got), want) {
							t.Fatalf("limit %d = %v\nwant the first %d of limit %d: %v", k, ids(got), k, n, want)
						}
						for j := range got {
							if got[j].Score != all[j].Score {
								t.Fatalf("limit %d: score of %s = %v, want %v", k, got[j].Page.ID, got[j].Score, all[j].Score)
							}
						}
					}
					for j := 1; j < len(all); j++ {
						if all[j].Score > all[j-1].Score {
							t.Fatalf("not sorted by final score at %d: %v > %v", j, all[j].Score, all[j-1].Score)
						}
					}
					for _, r := range all {
						if o := r.Page.PrivateOwner(); o != "" && !v.CanSeeOwner(o) {
							t.Errorf("hit %q is private to %q", r.Page.ID, o)
						}
					}
				})
			}
		}
	}
}

// Both paths snippet the hits they return: the boosted one, where the
// query is wrapped in a custom score, and the unboosted one, where it is
// not. A hit whose body carries the term must show it highlighted.
func TestTypeBoosts_SurvivorsCarrySnippets(t *testing.T) {
	for name, opts := range map[string][]Option{
		"boosted":   nil,
		"unboosted": {WithTypeBoosts(map[string]float64{})},
	} {
		t.Run(name, func(t *testing.T) {
			idx := newTestIndex(t, boostCorpus(), opts...)
			res, err := idx.Query("rollback", 5)
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 5 {
				t.Fatalf("got %d hits, want 5", len(res))
			}
			checked := 0
			for _, r := range res {
				if !strings.Contains(r.Page.Body, "rollback") {
					continue // a title-only match keeps the best-effort body head
				}
				checked++
				if !strings.Contains(r.Snippet, "<mark>rollback</mark>") {
					t.Errorf("%s: snippet %q has no highlighted match", r.Page.ID, r.Snippet)
				}
			}
			if checked == 0 {
				t.Fatal("fixture: no hit has the term in its body")
			}
		})
	}
}

// pointerAt is fixturePage as a pointer.
func pointerAt(id, title, body string) kb.Page {
	p := fixturePage(id, title, body, "pointers")
	p.Front.Type = kb.TypePointer
	p.Front.Target = "https://example.org/" + id
	return p
}

// boostCorpus is a deterministic mixed corpus: thin pointers, skills,
// longer content pages, and private pages another principal owns. Every
// page draws its words from one small vocabulary, so any query matches
// many pages with many distinct raw scores — the case where a cut before
// boosting shows.
func boostCorpus() []kb.Page {
	vocab := []string{"rollback", "canary", "incident", "latency", "budget", "quorum", "replica", "cache", "deploy", "alert"}
	r := rand.New(rand.NewSource(87)) //nolint:gosec // deterministic fixture, not security
	words := func(n int) string {
		out := make([]string, n)
		for k := range out {
			if r.Intn(3) == 0 {
				out[k] = vocab[r.Intn(len(vocab))]
			} else {
				out[k] = "filler"
			}
		}
		return strings.Join(out, " ")
	}
	var pages []kb.Page
	for k := range 15 {
		pages = append(pages, pointerAt(fmt.Sprintf("pointers/p%02d", k), "Pointer "+words(2), words(3)))
	}
	for k := range 6 {
		p := fixturePage(fmt.Sprintf("skills/s%02d", k), "Skill "+words(2), words(12), "skills")
		p.Front.Type = kb.TypeSkill
		pages = append(pages, p)
	}
	for k := range 25 {
		pages = append(pages, fixturePage(fmt.Sprintf("concepts/c%02d", k), "Concept "+words(3), words(30), "concepts"))
	}
	for k := range 6 {
		pages = append(pages, privatePage("alice-1111111111111111", fmt.Sprintf("n%02d", k), "Alice "+words(2), words(20)))
	}
	return pages
}
