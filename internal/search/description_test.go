package search

import (
	"context"
	"strings"
	"testing"

	"github.com/blevesearch/bleve/v2/search"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// description_test.go covers the OKF frontmatter `description` as a
// searchable field (issue #83): a page whose only occurrence of a term
// is its one-line summary has to be findable, at every stage, without
// the description ever outranking a page whose title or ID is the query.

// describedPage is fixturePage plus a frontmatter description.
func describedPage(id, title, body, description string) kb.Page {
	p := fixturePage(id, title, body, "concepts")
	p.Front.Description = description
	return p
}

// descriptionCorpus holds "capybara" in one page's description and
// nowhere else: not in a title, not in an ID, not in a body.
func descriptionCorpus() []kb.Page {
	return []kb.Page{
		describedPage("concepts/eviction", "Cache eviction", "Entries leave the store when it fills up.",
			"How the capybara watermark decides which entry goes first."),
		describedPage("concepts/sharding", "Sharding", "Data is split across shards by key hash.", ""),
	}
}

// A term present only in the description is reachable from every staged
// path: exact, then fuzzy for a typo, then prefix for a half-typed word.
func TestDescription_FoundAtEveryStage(t *testing.T) {
	idx := newTestIndex(t, descriptionCorpus())
	for _, tc := range []struct {
		name  string
		query string
		want  Stage
	}{
		{"exact term", "capybara", StageExact},
		{"one typo falls back to fuzzy", "capybra", StageFuzzy},
		{"a short word falls back to prefix", "capy", StagePrefix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, stage, err := idx.QueryStaged(context.Background(), kb.Unfiltered(), tc.query, 10)
			if err != nil {
				t.Fatalf("QueryStaged(%q): %v", tc.query, err)
			}
			if stage != tc.want {
				t.Errorf("stage = %s, want %s (res=%v)", stage, tc.want, ids(res))
			}
			if len(res) != 1 || res[0].Page.ID != "concepts/eviction" {
				t.Fatalf("res = %v, want just concepts/eviction", ids(res))
			}
			if res[0].Stage != tc.want {
				t.Errorf("Result.Stage = %s, want %s", res[0].Stage, tc.want)
			}
		})
	}
}

// The boost tiers: title (×5) beats description (×2) beats body (×1).
// Both pairs are mirror images — the same two strings, swapped between
// the fields — so the only thing separating them is which field the
// query matched in.
func TestDescription_BoostSitsBetweenTitleAndBody(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pages   []kb.Page
		query   string
		wantTop string
	}{
		{
			name: "a title match outranks a description-only match",
			pages: []kb.Page{
				describedPage("a/description", "Marsupials", "These animals are social grazers.", "Kangaroo behaviour in the outback."),
				describedPage("b/title", "Kangaroo behaviour", "These animals are social grazers.", "Marsupial behaviour in the outback."),
			},
			query:   "kangaroo",
			wantTop: "b/title",
		},
		{
			name: "a description match outranks a body-only match",
			pages: []kb.Page{
				describedPage("a/description", "Alpha notes", "A shared filler sentence for both pages.", "The quorum rule."),
				describedPage("b/body", "Beta notes", "The quorum rule.", "A shared filler sentence for both pages."),
			},
			query:   "quorum",
			wantTop: "a/description",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := newTestIndex(t, tc.pages)
			res, err := idx.Query(tc.query, 10)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(res) != 2 {
				t.Fatalf("res = %v, want both pages to match", ids(res))
			}
			if res[0].Page.ID != tc.wantTop {
				t.Fatalf("top hit = %q, want %q (scores %.4f vs %.4f)", res[0].Page.ID, tc.wantTop, res[0].Score, res[1].Score)
			}
			if !(res[0].Score > res[1].Score) {
				t.Errorf("top score %.4f is not above the next %.4f", res[0].Score, res[1].Score)
			}
		})
	}
}

// A description-only hit still carries a snippet: the body has nothing
// to highlight, so the description supplies the fragment.
func TestDescription_SuppliesTheSnippetWhenTheBodyHasNone(t *testing.T) {
	idx := newTestIndex(t, descriptionCorpus())

	res, err := idx.Query("capybara", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("res = %v, want one hit", ids(res))
	}
	if !strings.Contains(strings.ToLower(res[0].Snippet), "capybara") {
		t.Errorf("snippet = %q, want the description fragment around the match", res[0].Snippet)
	}

	// A body match still snippets from the body, not the description.
	res, err = idx.Query("shards", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) != 1 || !strings.Contains(strings.ToLower(res[0].Snippet), "shards") {
		t.Errorf("body match snippet = %q (res=%v)", res[0].Snippet, ids(res))
	}
}

// snippetFor prefers the field the query actually matched in. Bleve
// hands back a fragment for every highlighted field, so the body's
// leading text is present even on a description-only hit — hence the
// choice is made from the locations, with the fragments as the
// best-effort fallback.
func TestSnippetFor_PrefersTheFieldThatMatched(t *testing.T) {
	matched := func(fields ...string) search.FieldTermLocationMap {
		locs := search.FieldTermLocationMap{}
		for _, f := range fields {
			locs[f] = search.TermLocationMap{"term": nil}
		}
		return locs
	}
	both := search.FieldFragmentMap{"body": {"body frag"}, descriptionField: {"description frag"}}
	for _, tc := range []struct {
		name      string
		fragments search.FieldFragmentMap
		locations search.FieldTermLocationMap
		want      string
	}{
		{"a body match snippets from the body", both, matched("body"), "body frag"},
		{"a description-only match snippets from the description", both, matched(descriptionField), "description frag"},
		{"a match in both prefers the body", both, matched("body", descriptionField), "body frag"},
		{"a title-only match keeps the best-effort body fragment", both, matched("title"), "body frag"},
		{"no fragment at all is the empty snippet", nil, matched("id"), ""},
		{"an empty fragment list is no fragment", search.FieldFragmentMap{"body": {}, descriptionField: {"description frag"}}, matched("body"), "description frag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit := &search.DocumentMatch{Fragments: tc.fragments, Locations: tc.locations}
			if got := snippetFor(hit); got != tc.want {
				t.Errorf("snippetFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// A page with no description behaves exactly as it did before the field
// existed: found by title and body, snippeted from the body, and not
// matched by a term it does not carry.
func TestDescription_EmptyDescriptionIsUnchanged(t *testing.T) {
	idx := newTestIndex(t, descriptionCorpus())
	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"title still matches", "sharding", []string{"concepts/sharding"}},
		{"body still matches", "hash", []string{"concepts/sharding"}},
		{"id still matches", "concepts", []string{"concepts/eviction", "concepts/sharding"}},
		{"a term nobody carries still misses", "wombat", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := idx.Query(tc.query, 10)
			if err != nil {
				t.Fatalf("Query(%q): %v", tc.query, err)
			}
			got := ids(res)
			if len(got) != len(tc.want) {
				t.Fatalf("hits = %v, want %v", got, tc.want)
			}
			for _, want := range tc.want {
				if !strings.Contains(strings.Join(got, ","), want) {
					t.Errorf("hits = %v, want %q among them", got, want)
				}
			}
		})
	}
	// The body snippet is still what a body match shows.
	res, err := idx.Query("shards", 10)
	if err != nil || len(res) != 1 || res[0].Snippet == "" {
		t.Fatalf("body match on an undescribed page: res=%v err=%v", ids(res), err)
	}
}

// The new clause sits inside the visibility conjunction like every other
// content clause: another owner's private page is not reachable through
// its description either.
func TestDescription_RespectsVisibility(t *testing.T) {
	private := privatePage("alice-1111111111111111", "note", "Alice note", "nothing useful in the body")
	private.Front.Description = "The capybara is alice's secret."
	idx := newTestIndex(t, append(descriptionCorpus(), private))

	for _, tc := range []struct {
		name   string
		viewer kb.Viewer
		want   int
	}{
		{"the owner sees both description hits", kb.AsOwner("alice-1111111111111111"), 2},
		{"another principal sees only the public one", kb.AsOwner("bob-2222222222222222"), 1},
		{"an unfiltered viewer sees both", kb.Unfiltered(), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := idx.QueryAs(context.Background(), tc.viewer, "capybara", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Fatalf("hits = %v, want %d", ids(got), tc.want)
			}
			for _, r := range got {
				if o := r.Page.PrivateOwner(); o != "" && !tc.viewer.CanSeeOwner(o) {
					t.Errorf("hit %q is private to %q", r.Page.ID, o)
				}
			}
		})
	}
}

// TestDescription_OneLineMentionVsDenseBody records the boost trade-off
// the #85 review measured, so it is a decision rather than an accident.
// Under TF-IDF a one-line field's length norm dwarfs a 300-word body's,
// so a page that only MENTIONS a term in its description outranks a page
// whose body is ABOUT it (five uses in 300 words). With description in
// `_all` as well, that margin was about 40×. Kept out of `_all` it is
// about 7×.
//
// The mk-mpe eval's dev split did not favour a lower weight: ×1 was
// within noise and ×0.5 lost MRR. So the ordering stands, and the test
// bounds the margin. If it fails because the margin grew, something put
// the double count back. If a scoring change (BM25, #101) makes the dense
// body win, re-derive the boost table in docs/SEARCH.md and update this
// test to match.
func TestDescription_OneLineMentionVsDenseBody(t *testing.T) {
	prose := func(n, seed int) string {
		vocab := strings.Fields("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu apple banana cherry damson elder fig grape hazel iris jasmine kale lemon mango nutmeg olive pepper quince radish sage thyme")
		out := make([]string, n)
		for i := range out {
			out[i] = vocab[(i*7+seed)%len(vocab)]
		}
		return strings.Join(out, " ")
	}
	// Neither ID nor title carries the term: only the fields under test do.
	idx := newTestIndex(t, []kb.Page{
		describedPage("concepts/replication", "Replicated writes", prose(295, 1)+" quorum quorum quorum quorum quorum", ""),
		describedPage("concepts/storage-notes", "Storage notes", prose(300, 3), "How the quorum is chosen for replicated writes in the store."),
	})
	res, err := idx.Query("quorum", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("res = %v, want both pages found", ids(res))
	}
	if res[0].Page.ID != "concepts/storage-notes" {
		t.Fatalf("top = %s; the recorded trade-off is that a description mention outranks a dense body (see the comment)", res[0].Page.ID)
	}
	if ratio := res[0].Score / res[1].Score; ratio > 10 {
		t.Errorf("description-only / dense-body score ratio = %.1f, want <= 10 (about 7 with description out of _all; about 40 with it in)", ratio)
	}
}
