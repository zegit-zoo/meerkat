package collections

import (
	"context"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/search"
)

// role_test.go pins #95: a collection's default type boosts follow its
// ROLE. A routing tier (a tree root, or a collection that declares
// itself a hub with `layout.analyzer: ngram`) ranks with
// search.DefaultTypeBoosts, so its pointers outrank its own content.
// Everything else is a leaf and ranks with no type boost. An explicit
// `search.type_boosts` always wins, in both directions.
//
// The fixture is typeBoostPages: the notes page matches the query best,
// and the pointer outranks it only through the × 4.

func topHit(t *testing.T, c *Collection) string {
	t.Helper()
	reg, err := New(c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	hits, err := reg.Search(context.Background(), c.Name, "flux helmrelease drift", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	return hits[0].Page.ID
}

func TestTypeBoosts_FollowTheCollectionsRole(t *testing.T) {
	const pointerWins, pageWins = "hub/flux", "notes/flux"
	cases := []struct {
		name    string
		shape   func(*Collection)
		wantHub bool
		wantTop string
	}{
		{
			name:    "a plain collection is a leaf",
			shape:   func(*Collection) {},
			wantHub: false, wantTop: pageWins,
		},
		{
			name: "a tree root is a hub",
			shape: func(c *Collection) {
				c.Tree = &contentsource.TreeNode{Name: c.Name, Path: c.Name, Depth: 0}
			},
			wantHub: true, wantTop: pointerWins,
		},
		{
			name: "a tree child is a leaf",
			shape: func(c *Collection) {
				c.Tree = &contentsource.TreeNode{Name: c.Name, Path: "root/" + c.Name, Depth: 1, Parent: "root"}
			},
			wantHub: false, wantTop: pageWins,
		},
		{
			name: "an ngram title analyzer declares a hub",
			shape: func(c *Collection) {
				c.Source.Layout.Analyzer = search.AnalyzerNgram
			},
			wantHub: true, wantTop: pointerWins,
		},
		{
			name: "an explicit boost on a leaf wins",
			shape: func(c *Collection) {
				c.Source.Search = &contentsource.SearchSpec{TypeBoosts: map[string]float64{kb.TypePointer: 4}}
			},
			wantHub: false, wantTop: pointerWins,
		},
		{
			name: "an explicit empty map on a hub wins",
			shape: func(c *Collection) {
				c.Tree = &contentsource.TreeNode{Name: c.Name, Path: c.Name, Depth: 0}
				c.Source.Search = &contentsource.SearchSpec{TypeBoosts: map[string]float64{}}
			},
			wantHub: true, wantTop: pageWins,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := FromPages("kb", typeBoostPages())
			tc.shape(c)
			if got := c.IsHub(); got != tc.wantHub {
				t.Errorf("IsHub = %v, want %v", got, tc.wantHub)
			}
			if got := topHit(t, c); got != tc.wantTop {
				t.Errorf("top hit = %s, want %s", got, tc.wantTop)
			}
		})
	}
}

// Through a real tree resolution: the root keeps the defaults and a
// child does not, with no search: block anywhere.
func TestTypeBoosts_TreeRootAndChildFromManifests(t *testing.T) {
	reg := openTree(t)
	root, err := reg.Get("root")
	if err != nil {
		t.Fatal(err)
	}
	platform, err := reg.Get("platform")
	if err != nil {
		t.Fatal(err)
	}
	if !root.IsHub() {
		t.Error("the tree root is not a hub")
	}
	if platform.IsHub() {
		t.Error("a tree child is a hub")
	}
}
