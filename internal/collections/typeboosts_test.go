package collections

import (
	"context"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/contentsource"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

// typeBoostPages is the fixture internal/search pins its type-boost
// defaults with: a plain notes page whose body matches best, and a
// pointer that only outranks it through the ×4 pointer multiplier.
func typeBoostPages() []kb.Page {
	return []kb.Page{
		{ID: "notes/flux", Title: "Flux drift notes", Body: "flux helmrelease drift happens when the chart changes under you; see the flux docs"},
		{ID: "hub/flux", Title: "Flux CD", Body: "flux helmrelease kustomization drift gitops",
			Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:flux", Hint: "Flux CD."}},
	}
}

// TestSearch_TypeBoostsArePerCollection is issue #88's acceptance test at
// the registry: a collection's `search.type_boosts` reaches ITS index and
// nobody else's. The same two pages are mounted twice — once as a hub on
// the defaults, once as a leaf with pointers unboosted — and the same
// query ranks them in opposite orders.
func TestSearch_TypeBoostsArePerCollection(t *testing.T) {
	hub := FromPages("hub", typeBoostPages())
	leaf := FromPages("leaf", typeBoostPages())
	leaf.Source.Search = &contentsource.SearchSpec{TypeBoosts: map[string]float64{kb.TypePointer: 1}}
	off := FromPages("off", typeBoostPages())
	off.Source.Search = &contentsource.SearchSpec{TypeBoosts: map[string]float64{}}
	reg, err := New(hub, leaf, off)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	for _, tc := range []struct {
		collection string
		wantTop    string
	}{
		{"hub", "hub/flux"},    // no search: block — DefaultTypeBoosts, the pointer wins
		{"leaf", "notes/flux"}, // pointer: 1 — the page that matches best wins
		{"off", "notes/flux"},  // type_boosts: {} — boosting disabled outright
	} {
		t.Run(tc.collection, func(t *testing.T) {
			hits, err := reg.Search(context.Background(), tc.collection, "flux helmrelease drift", 10)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(hits) != 2 {
				t.Fatalf("got %d hits, want 2", len(hits))
			}
			if hits[0].Page.ID != tc.wantTop {
				t.Errorf("top hit in %s = %s, want %s", tc.collection, hits[0].Page.ID, tc.wantTop)
			}
		})
	}
}
