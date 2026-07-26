package cli

import (
	"slices"
	"strings"
	"testing"
)

// TestCompleteSourceIDs returns every source id from the embedded
// sources.yaml when the prefix is empty, and narrows on prefix.
func TestCompleteSourceIDs(t *testing.T) {
	all, _ := completeSourceIDs(nil, nil, "")
	if len(all) == 0 {
		t.Skip("no sources embedded")
	}
	if !slices.Contains(all, "policies") {
		t.Errorf("expected 'policies' in: %v", all)
	}
	if !slices.Contains(all, "rfc") {
		t.Errorf("expected 'rfc' in: %v", all)
	}

	// Prefix narrows to backend/frontend systems.
	systems, _ := completeSourceIDs(nil, nil, "back")
	for _, s := range systems {
		if !strings.HasPrefix(s, "back") {
			t.Errorf("non-prefix match: %q", s)
		}
	}
}

// TestCompletePageIDs without onlyStale should list every page;
// with onlyStale only placeholder/ingest-failed pages.
func TestCompletePageIDs(t *testing.T) {
	all, _ := completePageIDs(false)(nil, nil, "")
	if len(all) == 0 {
		t.Skip("no kb content embedded")
	}

	// Prefix narrows.
	concepts, _ := completePageIDs(false)(nil, nil, "concepts/")
	for _, p := range concepts {
		if !strings.HasPrefix(p, "concepts/") {
			t.Errorf("non-prefix match: %q", p)
		}
	}

	// onlyStale should be a strict subset of all pages.
	stale, _ := completePageIDs(true)(nil, nil, "")
	if len(stale) > len(all) {
		t.Errorf("stale (%d) > all (%d)", len(stale), len(all))
	}
}

// TestCompletePrefixes generates progressive slash segments.
func TestCompletePrefixes(t *testing.T) {
	all, _ := completePrefixes(nil, nil, "")
	// All entries end with /
	for _, p := range all {
		if !strings.HasSuffix(p, "/") {
			t.Errorf("prefix %q missing trailing /", p)
		}
	}
	if len(all) == 0 {
		t.Skip("no kb content embedded")
	}
	// systems/ should be present (any backend page would create it).
	if !slices.Contains(all, "systems/") {
		t.Errorf("expected 'systems/' in prefixes")
	}
}

// TestCompleteStatuses returns the documented enum, narrowable.
func TestCompleteStatuses(t *testing.T) {
	all, _ := completeStatuses(nil, nil, "")
	for _, want := range []string{"placeholder", "reviewed", "stale", "ingest-failed"} {
		if !slices.Contains(all, want) {
			t.Errorf("expected %q in statuses: %v", want, all)
		}
	}
	// 'p' narrows to placeholder.
	narrowed, _ := completeStatuses(nil, nil, "p")
	if !slices.Contains(narrowed, "placeholder") || slices.Contains(narrowed, "reviewed") {
		t.Errorf("prefix 'p' should narrow to placeholder, got: %v", narrowed)
	}
}

// TestCompleteCategories returns distinct frontmatter categories.
func TestCompleteCategories(t *testing.T) {
	all, _ := completeCategories(nil, nil, "")
	if len(all) == 0 {
		// No categories means no kb content embedded (content stripped).
		//
		t.Skip("no categories — no kb content embedded")
	}
	for _, want := range []string{"concepts", "systems", "policies"} {
		if !slices.Contains(all, want) {
			t.Errorf("expected %q in categories: %v", want, all)
		}
	}
}
