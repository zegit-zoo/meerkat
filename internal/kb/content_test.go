package kb

import (
	"errors"
	"strings"
	"testing"
)

// TestList exercises the happy path: at least one page is embedded
// and the list is sorted by ID.
// skipIfNoPages skips a content-dependent test when no pages are embedded
// (e.g. a build with the content source stripped).
func skipIfNoPages(t *testing.T) {
	t.Helper()
	pages, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pages) == 0 {
		t.Skip("no pages embedded")
	}
}

func TestList(t *testing.T) {
	skipIfNoPages(t)
	pages, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	for i := 1; i < len(pages); i++ {
		if pages[i-1].ID > pages[i].ID {
			t.Fatalf("pages not sorted: %q > %q", pages[i-1].ID, pages[i].ID)
		}
	}
}

// TestLoad_KnownPage verifies the canonical entry-point page loads.
func TestLoad_KnownPage(t *testing.T) {
	skipIfNoPages(t)
	page, err := Load("index")
	if err != nil {
		t.Fatalf("Load(index): %v", err)
	}
	if page.ID != "index" {
		t.Errorf("ID = %q, want %q", page.ID, "index")
	}
	if page.Body == "" {
		t.Error("Body is empty")
	}
	if page.Title == "" {
		t.Error("Title is empty")
	}
}

// TestLoad_AcceptsVariousIDForms ensures users don't have to know
// the exact spelling — leading slashes and .md suffixes are
// tolerated.
func TestLoad_AcceptsVariousIDForms(t *testing.T) {
	skipIfNoPages(t)
	cases := []string{"index", "/index", "index.md", "/index.md"}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			if _, err := Load(id); err != nil {
				t.Errorf("Load(%q): %v", id, err)
			}
		})
	}
}

// TestLoad_NotFound ensures missing pages return ErrNotFound.
func TestLoad_NotFound(t *testing.T) {
	_, err := Load("does-not-exist-anywhere")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestLoad_NestedPath verifies subdirectories work, e.g. concepts/X.
func TestLoad_NestedPath(t *testing.T) {
	pages, err := List()
	if err != nil {
		t.Fatal(err)
	}
	var nested string
	for _, p := range pages {
		if strings.Contains(p.ID, "/") {
			nested = p.ID
			break
		}
	}
	if nested == "" {
		t.Skip("no nested pages found, skipping")
	}
	if _, err := Load(nested); err != nil {
		t.Errorf("Load(%q): %v", nested, err)
	}
}

// TestExcluded ensures lint-report.md is filtered if present.
func TestExcluded(t *testing.T) {
	_, err := Load("lint-report")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("lint-report should be excluded, got err=%v", err)
	}
}

// TestExtractTitle covers the heading parser.
func TestExtractTitle(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"h1 first line", "# Hello\nbody", "Hello"},
		{"h2 first heading", "no heading\n## Section\nmore", "Section"},
		{"no headings", "just text", "fallback-id"},
		{"heading with spaces", "#   Padded   \n", "Padded"},
		{"empty heading falls through", "#\n# Real\n", "Real"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractTitle(tc.body, "fallback-id")
			if got != tc.want {
				t.Errorf("extractTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- frontmatter parsing (meerkat-specific delta) -------------

// TestSplitFrontmatter_Empty: no frontmatter, body returned verbatim.
func TestSplitFrontmatter_Empty(t *testing.T) {
	in := "# Hello\n\nplain body\n"
	fm, body := splitFrontmatter(in)
	if fm.ID != "" || fm.Status != "" {
		t.Errorf("expected zero Frontmatter, got %+v", fm)
	}
	if body != in {
		t.Errorf("body diverged from input")
	}
}

// TestSplitFrontmatter_Basic: standard YAML block, body follows.
func TestSplitFrontmatter_Basic(t *testing.T) {
	in := `---
id: policies/foo
title: Foo
category: policies
status: reviewed
owner: team-payments
tags: [psd2, tier-1]
---

# Foo body
`
	fm, body := splitFrontmatter(in)
	if fm.ID != "policies/foo" {
		t.Errorf("ID = %q", fm.ID)
	}
	if fm.Title != "Foo" {
		t.Errorf("Title = %q", fm.Title)
	}
	if fm.Category != "policies" {
		t.Errorf("Category = %q", fm.Category)
	}
	if fm.Status != "reviewed" {
		t.Errorf("Status = %q", fm.Status)
	}
	if fm.Owner != "team-payments" {
		t.Errorf("Owner = %q", fm.Owner)
	}
	if len(fm.Tags) != 2 || fm.Tags[0] != "psd2" {
		t.Errorf("Tags = %v", fm.Tags)
	}
	if !strings.HasPrefix(body, "\n# Foo body") {
		t.Errorf("body lost frontmatter trim, got: %q", body[:min(40, len(body))])
	}
}

// TestSplitFrontmatter_Source verifies nested Source struct decode.
func TestSplitFrontmatter_Source(t *testing.T) {
	in := `---
id: systems/backend/foo
source:
  type: gitlab
  repo: example/backend/foo
  ref: HEAD
  path: README.md
  web_url: https://gitlab.com/example/backend/foo
---
body
`
	fm, _ := splitFrontmatter(in)
	if fm.Source.Type != "gitlab" {
		t.Errorf("Source.Type = %q", fm.Source.Type)
	}
	if fm.Source.Repo != "example/backend/foo" {
		t.Errorf("Source.Repo = %q", fm.Source.Repo)
	}
	if fm.Source.WebURL == "" {
		t.Errorf("Source.WebURL is empty")
	}
}

// TestSplitFrontmatter_Extra: unknown top-level fields land in Extra and
// the typed core fields are decoded correctly alongside them.
func TestSplitFrontmatter_Extra(t *testing.T) {
	in := `---
id: policies/bar
title: Bar
category: policies
status: reviewed
regulator: FI
tier: gold
superseded_by: policies/baz
custom_field: hello
---
body
`
	fm, body := splitFrontmatter(in)
	// Core fields must be typed correctly.
	if fm.ID != "policies/bar" {
		t.Errorf("ID = %q", fm.ID)
	}
	if fm.Category != "policies" {
		t.Errorf("Category = %q", fm.Category)
	}
	if fm.Status != "reviewed" {
		t.Errorf("Status = %q", fm.Status)
	}
	// Domain-leaning fields must NOT be on the typed struct (removed fields).
	// They should live in Extra instead.
	if fm.Extra == nil {
		t.Fatalf("Extra is nil; expected domain-leaning fields to be collected there")
	}
	if v, ok := fm.Extra["regulator"]; !ok || v != "FI" {
		t.Errorf("Extra[regulator] = %v, want FI", fm.Extra["regulator"])
	}
	if _, ok := fm.Extra["tier"]; !ok {
		t.Errorf("Extra[tier] missing")
	}
	if _, ok := fm.Extra["superseded_by"]; !ok {
		t.Errorf("Extra[superseded_by] missing")
	}
	if _, ok := fm.Extra["custom_field"]; !ok {
		t.Errorf("Extra[custom_field] missing")
	}
	// Core keys must NOT leak into Extra.
	for _, coreKey := range []string{"id", "title", "category", "status"} {
		if _, bad := fm.Extra[coreKey]; bad {
			t.Errorf("core key %q leaked into Extra", coreKey)
		}
	}
	if !strings.HasPrefix(body, "body") {
		t.Errorf("unexpected body: %q", body)
	}
}

// TestSplitFrontmatter_ExtraCoreOnly: when all fields are core, Extra is nil.
func TestSplitFrontmatter_ExtraCoreOnly(t *testing.T) {
	in := `---
id: test/page
title: Page
status: reviewed
---
body
`
	fm, _ := splitFrontmatter(in)
	if fm.Extra != nil {
		t.Errorf("Extra should be nil when only core keys present, got %v", fm.Extra)
	}
}

// TestSplitFrontmatter_Malformed: invalid YAML returns empty
// Frontmatter + full body, never errors.
func TestSplitFrontmatter_Malformed(t *testing.T) {
	in := "---\n: [unbalanced\n---\n# body\n"
	fm, body := splitFrontmatter(in)
	if fm.ID != "" || fm.Status != "" {
		t.Errorf("malformed YAML should yield zero frontmatter, got %+v", fm)
	}
	if body == "" {
		t.Error("body should not be empty even on bad frontmatter")
	}
}

// TestFilterHelpers exercises the public Filter / By* presets that
// the CLI/MCP/HTTP layers depend on.
func TestFilterHelpers(t *testing.T) {
	pages, err := List()
	if err != nil {
		t.Fatal(err)
	}

	// ByPrefix("") == nil — wildcard semantics expected by callers.
	if kept := Filter(pages, ByPrefix("")); len(kept) != len(pages) {
		t.Errorf("ByPrefix(\"\") should be a no-op; got %d / %d", len(kept), len(pages))
	}

	// ByCategory: filter to one category and verify membership.
	conceptPages := Filter(pages, ByCategory("concepts"))
	for _, p := range conceptPages {
		if p.Front.Category != "concepts" {
			t.Errorf("ByCategory leak: %s has category %q", p.ID, p.Front.Category)
		}
	}

	// ByPrefix narrows correctly.
	systems := Filter(pages, ByPrefix("systems/"))
	for _, p := range systems {
		if !strings.HasPrefix(p.ID, "systems/") {
			t.Errorf("ByPrefix leak: %s", p.ID)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
