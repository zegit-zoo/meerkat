package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/kb"
)

func linkRegistry(t *testing.T) *collections.Registry {
	t.Helper()
	hub := collections.FromPages("hub", []kb.Page{
		{ID: "flux", Title: "Flux CD", Body: "flux helmrelease drift gitops", Front: kb.Frontmatter{Type: kb.TypePointer, Target: "collection:flux", Hint: "GitOps.", Related: []string{"skills/diff"}}},
		{ID: "skills/diff", Title: "Run flux diff", Body: "flux diff shows drift", Front: kb.Frontmatter{Type: kb.TypeSkill}},
		{ID: "notes", Title: "Flux notes", Body: "flux drift notes", Front: kb.Frontmatter{Related: []string{"flux", "nope"}}},
	})
	flux := collections.FromPages("flux", []kb.Page{
		{ID: "drift", Title: "Drift", Body: "flux helmrelease drift", Front: kb.Frontmatter{Related: []string{"hub:flux"}}},
	})
	reg, err := collections.New(hub, flux)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestSearchWire_CarriesKindAndPointerFields(t *testing.T) {
	reg := linkRegistry(t)
	hits, err := reg.Search(t.Context(), "", "flux drift", 10)
	if err != nil {
		t.Fatal(err)
	}
	body, err := searchResultsJSON(hits)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out[0]["kind"] != "pointer" || out[0]["target"] != "collection:flux" || out[0]["target_resolved"] != true || out[0]["hint"] != "GitOps." {
		t.Errorf("top hit = %v", out[0])
	}
	for _, h := range out[1:] {
		if _, has := h["target"]; has {
			t.Errorf("non-pointer hit carries target: %v", h)
		}
		if h["kind"] == nil || h["type"] == nil {
			t.Errorf("hit lacks kind/type: %v", h)
		}
	}

	bundled, err := bundlesJSON("flux drift", reg.Bundles(hits))
	if err != nil {
		t.Fatal(err)
	}
	var b struct {
		Query   string `json:"query"`
		Bundles []struct {
			Target   string           `json:"target"`
			Hint     string           `json:"hint"`
			Pointers []map[string]any `json:"pointers"`
			Skills   []map[string]any `json:"skills"`
			Docs     []map[string]any `json:"docs"`
		} `json:"bundles"`
	}
	if err := json.Unmarshal([]byte(bundled), &b); err != nil {
		t.Fatal(err)
	}
	if b.Query != "flux drift" || len(b.Bundles) < 2 || b.Bundles[0].Target != "collection:flux" || len(b.Bundles[0].Pointers) != 1 || len(b.Bundles[0].Skills) != 1 {
		t.Errorf("bundles = %s", bundled)
	}
	if last := b.Bundles[len(b.Bundles)-1]; last.Target != "" || len(last.Docs) == 0 {
		t.Errorf("unrouted hits must land in the empty-target bundle: %s", bundled)
	}
}

func TestShowWire_CarriesLinksAndBacklinks(t *testing.T) {
	reg := linkRegistry(t)
	ref, err := reg.Show("hub", "flux")
	if err != nil {
		t.Fatal(err)
	}
	body, err := showPageJSON(reg, ref)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out["kind"] != "pointer" {
		t.Errorf("kind = %v", out["kind"])
	}
	ptr, _ := out["pointer"].(map[string]any)
	if ptr["resolved"] != true || ptr["collection"] != "flux" {
		t.Errorf("pointer = %v", out["pointer"])
	}
	from, _ := out["linked_from"].([]any)
	joined := ""
	for _, f := range from {
		joined += f.(string) + ","
	}
	if !strings.Contains(joined, "flux:drift") || !strings.Contains(joined, "hub:notes") {
		t.Errorf("linked_from = %v", out["linked_from"])
	}

	notes, _ := reg.Show("hub", "notes")
	body, _ = showPageJSON(reg, notes)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	links, _ := out["links"].([]any)
	if len(links) != 2 || links[1].(map[string]any)["resolved"] != false || !strings.Contains(links[1].(map[string]any)["reason"].(string), "not found") {
		t.Errorf("links = %v", out["links"])
	}
	if out["kind"] != "doc" {
		t.Errorf("kind = %v, want doc", out["kind"])
	}
}
