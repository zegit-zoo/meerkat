package search

import (
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

func TestTypeBoosts_PointerOutranksThinContentByDefault(t *testing.T) {
	pages := []kb.Page{
		{ID: "notes/flux", Title: "Flux drift notes", Body: "flux helmrelease drift happens when the chart changes under you; see the flux docs", Front: kb.Frontmatter{}},
		{ID: "hub/flux", Title: "Flux CD", Body: "flux helmrelease kustomization drift gitops", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:flux", Hint: "Flux CD."}},
		{ID: "skills/flux-drift", Title: "Diagnose it", Body: "flux helmrelease drift: run flux diff", Front: kb.Frontmatter{Type: kb.TypeSkill}},
	}
	idx, err := NewFromPages(pages)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	res, err := idx.Query("flux helmrelease drift", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 || res[0].Page.ID != "hub/flux" {
		t.Fatalf("default boosts: order = %v, want the pointer first", ids(res))
	}

	// Disabled: the plain page with the longest matching body wins.
	off, err := NewFromPages(pages, WithTypeBoosts(map[string]float64{}))
	if err != nil {
		t.Fatal(err)
	}
	defer off.Close()
	res, _ = off.Query("flux helmrelease drift", 10)
	if len(res) != 3 || res[0].Page.ID == "hub/flux" {
		t.Errorf("with boosts disabled the pointer must not be lifted by its type: %v", ids(res))
	}

	// Custom: examples above everything.
	custom, err := NewFromPages(pages, WithTypeBoosts(map[string]float64{kb.TypeSkill: 10}))
	if err != nil {
		t.Fatal(err)
	}
	defer custom.Close()
	res, _ = custom.Query("flux helmrelease drift", 10)
	if res[0].Page.ID != "skills/flux-drift" {
		t.Errorf("custom boosts: order = %v", ids(res))
	}
}
