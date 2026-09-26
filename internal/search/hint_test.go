package search

import (
	"context"
	"strings"
	"testing"

	"github.com/blevesearch/bleve/v2/search"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// hint_test.go covers a pointer's `hint:` as a searchable field (issue
// #88): the one sentence saying why to follow a pointer is often the only
// place its subject is named in words a searcher types, so a term there
// has to find the pointer — at every stage, at the body's weight.

// pointerPage is fixturePage as a pointer with a target and a hint.
func pointerPage(id, title, body, hint string) kb.Page {
	p := fixturePage(id, title, body, "pointers")
	p.Front.Type = kb.TypePointer
	p.Front.Target = "https://example.org/" + id
	p.Front.Hint = hint
	return p
}

// hintCorpus holds "wombat" in one pointer's hint and nowhere else: not
// in a title, an ID, a description or a body.
func hintCorpus() []kb.Page {
	return []kb.Page{
		pointerPage("pointers/burrows", "Burrow engineering, chapter 4", "Chapter pointer.",
			"Why the wombat digs its tunnels in separated chambers."),
		fixturePage("concepts/tunnels", "Tunnels", "Tunnels are dug for shelter.", "concepts"),
	}
}

// The mk-mpe reproduction from #88: a hint-only term was not found at
// all. Now it is, through every staged path.
func TestHint_FoundAtEveryStage(t *testing.T) {
	idx := newTestIndex(t, hintCorpus())
	for _, tc := range []struct {
		name  string
		query string
		want  Stage
	}{
		{"exact term", "wombat", StageExact},
		{"the #88 reproduction term", "separated", StageExact},
		{"one typo falls back to fuzzy", "wombet", StageFuzzy},
		{"a short word falls back to prefix", "womb", StagePrefix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, stage, err := idx.QueryStaged(context.Background(), kb.Unfiltered(), tc.query, 10)
			if err != nil {
				t.Fatalf("QueryStaged(%q): %v", tc.query, err)
			}
			if stage != tc.want {
				t.Errorf("stage = %s, want %s (res=%v)", stage, tc.want, ids(res))
			}
			if len(res) != 1 || res[0].Page.ID != "pointers/burrows" {
				t.Fatalf("res = %v, want just pointers/burrows", ids(res))
			}
		})
	}
}

// `hint:` is addressable like any other analysed field.
func TestHint_FieldSyntax(t *testing.T) {
	idx := newTestIndex(t, hintCorpus())
	res, err := idx.Query("hint:wombat", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) != 1 || res[0].Page.ID != "pointers/burrows" {
		t.Fatalf("hint:wombat = %v", ids(res))
	}
	if res, _ := idx.Query("hint:shelter", 10); len(res) != 0 {
		t.Errorf("hint:shelter matched a body-only term: %v", ids(res))
	}
}

// Field weights with the type multiplier switched off, so only the field
// that matched separates the pages: a title beats a hint, and a
// hint-only match still beats a body-only one — the hint has a clause of
// its own on top of `_all`, where the body has only `_all`. Mirror-image
// fixtures, as in the description tests.
func TestHint_WeightsAgainstTitleAndBody(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pages   []kb.Page
		query   string
		wantTop string
	}{
		{
			name: "a title match outranks a hint-only match",
			pages: []kb.Page{
				pointerPage("a/hint", "Marsupials", "These animals are social grazers.", "Kangaroo behaviour in the outback."),
				pointerPage("b/title", "Kangaroo behaviour", "These animals are social grazers.", "Marsupial behaviour in the outback."),
			},
			query:   "kangaroo",
			wantTop: "b/title",
		},
		{
			name: "a hint match outranks a body-only match",
			pages: []kb.Page{
				pointerPage("a/hint", "Alpha notes", "A shared filler sentence for both pages.", "The quorum rule."),
				pointerPage("b/body", "Beta notes", "The quorum rule.", "A shared filler sentence for both pages."),
			},
			query:   "quorum",
			wantTop: "a/hint",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := newTestIndex(t, tc.pages, WithTypeBoosts(map[string]float64{}))
			res, err := idx.Query(tc.query, 10)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(res) != 2 {
				t.Fatalf("res = %v, want both pages to match", ids(res))
			}
			if res[0].Page.ID != tc.wantTop || !(res[0].Score > res[1].Score) {
				t.Fatalf("top hit = %q (%.4f vs %.4f), want %q strictly first", res[0].Page.ID, res[0].Score, res[1].Score, tc.wantTop)
			}
		})
	}
}

// A hint-only hit snippets from the hint, and a pointer indexed
// incrementally through Put carries its hint too (shared indexDoc).
func TestHint_SnippetAndPut(t *testing.T) {
	idx := newTestIndex(t, hintCorpus())
	res, err := idx.Query("wombat", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) != 1 || !strings.Contains(strings.ToLower(res[0].Snippet), "wombat") {
		t.Fatalf("snippet = %q (res=%v), want the hint fragment", snippetOf(res), ids(res))
	}

	if err := idx.Put(pointerPage("pointers/late", "Late pointer", "Added mid-session.", "The numbat eats termites.")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	res, err = idx.Query("numbat", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) != 1 || res[0].Page.ID != "pointers/late" {
		t.Fatalf("hint-only query after Put = %v", ids(res))
	}
}

// snippetFor falls through body, then description, then hint.
func TestSnippetFor_HintIsTheLastFallback(t *testing.T) {
	all := search.FieldFragmentMap{"body": {"body frag"}, descriptionField: {"description frag"}, hintField: {"hint frag"}}
	for _, tc := range []struct {
		name    string
		matched []string
		want    string
	}{
		{"a hint-only match snippets from the hint", []string{hintField}, "hint frag"},
		{"description is preferred over hint", []string{descriptionField, hintField}, "description frag"},
		{"body is preferred over both", []string{"body", descriptionField, hintField}, "body frag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locs := search.FieldTermLocationMap{}
			for _, f := range tc.matched {
				locs[f] = search.TermLocationMap{"term": nil}
			}
			if got := snippetFor(&search.DocumentMatch{Fragments: all, Locations: locs}); got != tc.want {
				t.Errorf("snippetFor = %q, want %q", got, tc.want)
			}
		})
	}
}

func snippetOf(res []Result) string {
	if len(res) == 0 {
		return ""
	}
	return res[0].Snippet
}
