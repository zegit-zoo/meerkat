package kb

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
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
	fm, body, present := splitFrontmatter(in)
	if fm.ID != "" || fm.Status != "" {
		t.Errorf("expected zero Frontmatter, got %+v", fm)
	}
	if body != in {
		t.Errorf("body diverged from input")
	}
	if present {
		t.Error("present should be false when there is no frontmatter block at all")
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
	fm, body, present := splitFrontmatter(in)
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
	if !present {
		t.Error("present should be true for a well-formed frontmatter block")
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
	fm, _, _ := splitFrontmatter(in)
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
	fm, body, _ := splitFrontmatter(in)
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
	fm, _, _ := splitFrontmatter(in)
	if fm.Extra != nil {
		t.Errorf("Extra should be nil when only core keys present, got %v", fm.Extra)
	}
}

// TestSplitFrontmatter_Malformed: invalid YAML returns empty
// Frontmatter + full body, never errors.
func TestSplitFrontmatter_Malformed(t *testing.T) {
	in := "---\n: [unbalanced\n---\n# body\n"
	fm, body, present := splitFrontmatter(in)
	if fm.ID != "" || fm.Status != "" {
		t.Errorf("malformed YAML should yield zero frontmatter, got %+v", fm)
	}
	if body == "" {
		t.Error("body should not be empty even on bad frontmatter")
	}
	if present {
		t.Error("present should be false when the YAML fails to parse")
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

// TestByType exercises the OKF `type` filter facet against synthetic
// pages, independent of whatever content happens to be embedded.
func TestByType(t *testing.T) {
	pages := []Page{
		{ID: "tables/orders", Front: Frontmatter{Type: "BigQuery Table"}},
		{ID: "tables/customers", Front: Frontmatter{Type: "BigQuery Table"}},
		{ID: "playbooks/oncall", Front: Frontmatter{Type: "Playbook"}},
		{ID: "notype/page", Front: Frontmatter{}},
	}
	tables := Filter(pages, ByType("BigQuery Table"))
	if len(tables) != 2 {
		t.Fatalf("ByType(BigQuery Table) = %d pages, want 2", len(tables))
	}
	for _, p := range tables {
		if p.Front.Type != "BigQuery Table" {
			t.Errorf("ByType leak: %s has type %q", p.ID, p.Front.Type)
		}
	}
	if got := Filter(pages, ByType("Nonexistent")); len(got) != 0 {
		t.Errorf("ByType(Nonexistent) = %d pages, want 0", len(got))
	}
}

// --- OKF (Open Knowledge Format) frontmatter fields ------------

// TestSplitFrontmatter_TypeAndDescriptionAreCore proves type/description
// decode onto the typed core fields and do NOT also appear in Extra —
// see the coreKeys comment: promoting a field into the struct must
// remove it from the Extra catch-all in the same change.
func TestSplitFrontmatter_TypeAndDescriptionAreCore(t *testing.T) {
	in := `---
type: BigQuery Table
description: One row per completed customer order.
custom_field: still-in-extra
---
body
`
	fm, _, present := splitFrontmatter(in)
	if !present {
		t.Fatal("expected frontmatter to be present")
	}
	if fm.Type != "BigQuery Table" {
		t.Errorf("Type = %q", fm.Type)
	}
	if fm.Description != "One row per completed customer order." {
		t.Errorf("Description = %q", fm.Description)
	}
	if _, bad := fm.Extra["type"]; bad {
		t.Error("type leaked into Extra")
	}
	if _, bad := fm.Extra["description"]; bad {
		t.Error("description leaked into Extra")
	}
	if _, ok := fm.Extra["custom_field"]; !ok {
		t.Error("custom_field (still unpromoted) should be in Extra")
	}
}

// TestSplitFrontmatter_ResourceAndSourcesStayInExtra proves the two
// deliberately-NOT-promoted OKF fields (resource, sources) keep landing
// in Extra, exactly like any other unknown key.
func TestSplitFrontmatter_ResourceAndSourcesStayInExtra(t *testing.T) {
	in := `---
type: BigQuery Table
resource: https://bigquery.googleapis.com/v2/projects/acme/datasets/sales/tables/orders
sources:
  - id: erp-export
    resource: https://erp.example.com/schema/orders
---
body
`
	fm, _, _ := splitFrontmatter(in)
	if _, ok := fm.Extra["resource"]; !ok {
		t.Error("resource should survive in Extra")
	}
	if _, ok := fm.Extra["sources"]; !ok {
		t.Error("sources should survive in Extra")
	}
}

// TestSplitFrontmatter_VerifiedList covers the canonical (non-bare)
// list shape (OKF SPEC.md §5.2).
func TestSplitFrontmatter_VerifiedList(t *testing.T) {
	in := `---
type: BigQuery Table
verified:
  - { by: human:ahormati, at: 2026-06-25T09:00:00Z }
  - { by: process:finance-nightly, at: 2026-06-26T02:00:00Z }
---
body
`
	fm, _, _ := splitFrontmatter(in)
	if len(fm.Verified) != 2 {
		t.Fatalf("Verified = %v, want 2 entries", fm.Verified)
	}
	if fm.Verified[0].By != "human:ahormati" {
		t.Errorf("Verified[0].By = %q", fm.Verified[0].By)
	}
	if fm.Verified[1].By != "process:finance-nightly" {
		t.Errorf("Verified[1].By = %q", fm.Verified[1].By)
	}
}

// TestSplitFrontmatter_VerifiedBareMapping covers OKF's "a single
// verifier MAY be written as one { by, at } mapping without the list
// dash" allowance — consumers MUST treat it as a one-element list
// (SPEC.md §5.2, §11).
func TestSplitFrontmatter_VerifiedBareMapping(t *testing.T) {
	in := `---
type: BigQuery Table
verified: { by: human:ahormati, at: 2026-06-25T09:00:00Z }
---
body
`
	fm, _, _ := splitFrontmatter(in)
	if len(fm.Verified) != 1 {
		t.Fatalf("Verified = %v, want exactly one element", fm.Verified)
	}
	if fm.Verified[0].By != "human:ahormati" {
		t.Errorf("Verified[0].By = %q", fm.Verified[0].By)
	}
	if fm.Verified[0].At != "2026-06-25T09:00:00Z" {
		t.Errorf("Verified[0].At = %q", fm.Verified[0].At)
	}
}

// TestSplitFrontmatter_VerifiedAbsent proves no verified key at all
// yields a nil/empty VerifiedList, not an error.
func TestSplitFrontmatter_VerifiedAbsent(t *testing.T) {
	in := "---\ntype: BigQuery Table\n---\nbody\n"
	fm, _, present := splitFrontmatter(in)
	if !present {
		t.Fatal("expected frontmatter to be present")
	}
	if len(fm.Verified) != 0 {
		t.Errorf("Verified = %v, want empty", fm.Verified)
	}
}

// TestSplitFrontmatter_VerifiedNull proves an explicit null scalar
// (`verified: ~`) degrades to "no verifiers" rather than an error.
func TestSplitFrontmatter_VerifiedNull(t *testing.T) {
	in := "---\ntype: BigQuery Table\nverified: ~\n---\nbody\n"
	fm, _, present := splitFrontmatter(in)
	if !present {
		t.Fatal("expected frontmatter to be present")
	}
	if len(fm.Verified) != 0 {
		t.Errorf("Verified = %v, want empty for a null scalar", fm.Verified)
	}
}

// TestSplitFrontmatter_VerifiedMalformedScalar proves a non-null bare
// scalar (neither a mapping nor a list) fails to parse — and, matching
// the existing malformed-YAML precedent (TestSplitFrontmatter_Malformed),
// degrades to "no frontmatter" for the whole page rather than a partial
// parse, so the page still loads instead of being rejected outright.
func TestSplitFrontmatter_VerifiedMalformedScalar(t *testing.T) {
	in := "---\ntype: BigQuery Table\nverified: not-a-mapping\n---\nbody\n"
	fm, body, present := splitFrontmatter(in)
	if present {
		t.Error("present should be false for an unparsable verified shape")
	}
	if fm.Type != "" {
		t.Errorf("expected zero Frontmatter, got %+v", fm)
	}
	if body != in {
		t.Error("body should fall back to the raw input")
	}
}

// TestTrustTier covers the three OKF trust tiers (SPEC.md §5.3) plus
// the actor-prefix edge case that only a "human:"-prefixed By counts
// as human review.
func TestTrustTier(t *testing.T) {
	cases := []struct {
		name string
		fm   Frontmatter
		want string
	}{
		{"no verified key", Frontmatter{}, TrustUnverified},
		{"empty verified list", Frontmatter{Verified: VerifiedList{}}, TrustUnverified},
		{
			"machine only", Frontmatter{Verified: VerifiedList{
				{By: "process:finance-nightly"},
				{By: "reference_agent/gemini-2.5-pro"},
			}}, TrustMachineConfirmed,
		},
		{
			"human present among many", Frontmatter{Verified: VerifiedList{
				{By: "process:finance-nightly"},
				{By: "human:ahormati"},
			}}, TrustHumanReviewed,
		},
		{
			"human only", Frontmatter{Verified: VerifiedList{{By: "human:ahormati"}}},
			TrustHumanReviewed,
		},
		{
			// A prefix match, not a bare equality — "humans:" or a
			// substring occurrence elsewhere must not false-positive.
			"lookalike actor is not human", Frontmatter{Verified: VerifiedList{{By: "humans:not-the-prefix"}}},
			TrustMachineConfirmed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fm.TrustTier(); got != tc.want {
				t.Errorf("TrustTier() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsStale covers OKF's "today >= stale_after" rule (SPEC.md §5.5),
// including the inclusive boundary and the two ways an absent/malformed
// StaleAfter must never manufacture a staleness signal.
func TestIsStale(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		staleAfter string
		want       bool
	}{
		{"empty is never stale", "", false},
		{"malformed date is never stale", "not-a-date", false},
		{"past date is stale", "2020-01-01", true},
		{"future date is not stale", "2099-01-01", false},
		{"exact boundary (today) is stale", "2026-07-26", true},
		{"day after tomorrow is not stale", "2026-07-28", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fm := Frontmatter{StaleAfter: tc.staleAfter}
			if got := fm.IsStale(now); got != tc.want {
				t.Errorf("IsStale(%q) at %s = %v, want %v", tc.staleAfter, now, got, tc.want)
			}
		})
	}
}

// --- reserved OKF filenames -------------------------------------

// TestIsReservedArtifact covers the frontmatter-presence discriminator
// directly, including nested paths (§3.1: reserved "at any level of
// the hierarchy") and filenames that must NEVER be treated as reserved
// regardless of frontmatter.
func TestIsReservedArtifact(t *testing.T) {
	cases := []struct {
		path           string
		hasFrontmatter bool
		want           bool
	}{
		{"content/index.md", false, true},
		{"content/log.md", false, true},
		{"content/tables/index.md", false, true},
		{"content/a/b/c/log.md", false, true},
		{"content/index.md", true, false},         // meerkat landing page w/ id:/title:
		{"content/log.md", true, false},           // frontmatter present -> not reserved
		{"content/concepts/foo.md", false, false}, // ordinary filename, never reserved
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/hasFrontmatter=%v", tc.path, tc.hasFrontmatter), func(t *testing.T) {
			if got := isReservedArtifact(tc.path, tc.hasFrontmatter); got != tc.want {
				t.Errorf("isReservedArtifact(%q, %v) = %v, want %v", tc.path, tc.hasFrontmatter, got, tc.want)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
