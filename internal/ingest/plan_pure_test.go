package ingest

import (
	"testing"

	"github.com/zegit-zoo/meerkat/internal/sources"
)

func TestIDBasename(t *testing.T) {
	cases := map[string]string{
		"policies/foo":          "foo",
		"foo":                   "foo",
		"systems/backend/api":   "api",
		"a/b/c/d":               "d",
		"":                      "",
		"operations/runbooks/x": "x",
	}
	for in, want := range cases {
		if got := idBasename(in); got != want {
			t.Errorf("idBasename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPagePathForRepo(t *testing.T) {
	if got := pagePathForRepo("wiki", "policies/incident-handling"); got != "wiki/policies/incident-handling.md" {
		t.Errorf("pagePathForRepo(wiki) = %q", got)
	}
	// Honors a custom wiki dir (content-source.yaml layout.wiki).
	if got := pagePathForRepo("docs", "concepts/rate-limiting"); got != "docs/concepts/rate-limiting.md" {
		t.Errorf("pagePathForRepo(docs) = %q", got)
	}
}

func TestFormatSource(t *testing.T) {
	s := sources.Source{
		Type:       "gitlab",
		Repo:       "your-org/architecture",
		Path:       "adr/",
		Enrichment: []string{"past-incidents", "service-catalog"},
	}
	got := formatSource(s)
	want := "type=gitlab repo=your-org/architecture path=adr/ enrichment=past-incidents,service-catalog"
	if got != want {
		t.Errorf("formatSource = %q, want %q", got, want)
	}
}

func TestFormatSource_GroupOnly(t *testing.T) {
	s := sources.Source{Type: "gitlab-group", Group: "your-org/requirements"}
	got := formatSource(s)
	want := "type=gitlab-group group=your-org/requirements"
	if got != want {
		t.Errorf("formatSource = %q, want %q", got, want)
	}
}

func TestFormatSource_Minimal(t *testing.T) {
	s := sources.Source{Type: "synthesised"}
	if got := formatSource(s); got != "type=synthesised" {
		t.Errorf("formatSource = %q, want %q", got, "type=synthesised")
	}
}
