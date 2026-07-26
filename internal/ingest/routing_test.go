package ingest

import (
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
	"github.com/zegit-zoo/meerkat/internal/sources"
)

func TestMatchSource(t *testing.T) {
	byCat := map[string]sources.Source{
		"adr":                 {ID: "adr", TargetCategory: "adr"},
		"concepts":            {ID: "concepts", TargetCategory: "concepts"},
		"operations/runbooks": {ID: "runbooks", TargetCategory: "operations/runbooks"},
		"systems/backend":     {ID: "backend", TargetCategory: "systems/backend"},
		"a":                   {ID: "a", TargetCategory: "a"},
		"a/b":                 {ID: "ab", TargetCategory: "a/b"},
	}
	page := func(id, cat, sub string) kb.Page {
		return kb.Page{ID: id, Front: kb.Frontmatter{Category: cat, Subcategory: sub}}
	}
	cases := []struct {
		name   string
		page   kb.Page
		wantID string
		wantOK bool
	}{
		{"plain category", page("adr/0001", "adr", ""), "adr", true},
		{"category/subcategory", page("operations/runbooks/db", "operations", "runbooks"), "runbooks", true},
		{"id prefix (was systems/* hardcode)", page("systems/backend/api", "systems", ""), "backend", true},
		{"longest prefix wins", page("a/b/c", "", ""), "ab", true},
		{"no match", page("random/x", "nope", ""), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchSource(tc.page, byCat)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.ID != tc.wantID {
				t.Errorf("matched %q, want %q", got.ID, tc.wantID)
			}
		})
	}
}

func TestFormatSource_IncludesHost(t *testing.T) {
	s := sources.Source{Type: "files", Host: "github", Repo: "your-org/arch"}
	if got := formatSource(s); got != "type=files host=github repo=your-org/arch" {
		t.Errorf("formatSource = %q", got)
	}
}
