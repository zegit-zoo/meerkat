package search

import (
	"strings"
	"testing"
)

// TestNew exercises index construction.
func TestNew(t *testing.T) {
	idx, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()
	if idx.bleve == nil {
		t.Fatal("bleve index is nil")
	}
	skipIfNoContent(t, idx)
}

// skipIfNoContent skips a content-dependent test when no kb content is
// embedded (e.g. a build with the content source stripped). See
// a build with no embedded KB content.
func skipIfNoContent(t *testing.T, idx *Index) {
	t.Helper()
	if len(idx.pages) == 0 {
		t.Skip("no kb content embedded")
	}
}

// TestQuery_FindsKnownTerm — a common domain term we expect across
// many pages (when content is embedded).
func TestQuery_FindsKnownTerm(t *testing.T) {
	idx := mustNewIndex(t)
	defer idx.Close()
	skipIfNoContent(t, idx)

	results, err := idx.Query("payment", 5)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one hit for 'payment'")
	}
}

// TestQuery_RankByTitle: a query that matches a page title outranks
// incidental body mentions.
func TestQuery_RankByTitle(t *testing.T) {
	idx := mustNewIndex(t)
	defer idx.Close()

	results, err := idx.Query("rate limiting", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Skip("rate-limiting pages not present in this snapshot")
	}
	// The concept or runbook page should outrank random body matches.
	top := strings.ToLower(results[0].Page.ID)
	if !strings.Contains(top, "rate") {
		t.Errorf("top hit ID = %q, expected to contain 'rate'", results[0].Page.ID)
	}
}

// TestQuery_EmptyQuery returns no results without erroring.
func TestQuery_EmptyQuery(t *testing.T) {
	idx := mustNewIndex(t)
	defer idx.Close()

	results, err := idx.Query("", 5)
	if err != nil {
		t.Errorf("empty query should not error, got %v", err)
	}
	if results != nil {
		t.Errorf("empty query should return nil results, got %d", len(results))
	}
}

// TestQuery_LimitClamping: zero / negative limits clamp to default 10.
func TestQuery_LimitClamping(t *testing.T) {
	idx := mustNewIndex(t)
	defer idx.Close()

	for _, lim := range []int{-1, 0} {
		results, err := idx.Query("payment", lim)
		if err != nil {
			t.Errorf("Query(limit=%d): %v", lim, err)
		}
		if len(results) > 10 {
			t.Errorf("limit=%d returned %d, expected default cap of 10", lim, len(results))
		}
	}
}

// TestQuery_HasSnippet: hits include a body snippet (Bleve highlight).
func TestQuery_HasSnippet(t *testing.T) {
	idx := mustNewIndex(t)
	defer idx.Close()

	results, err := idx.Query("payment", 3)
	if err != nil || len(results) == 0 {
		t.Skipf("no hits (err=%v) — skipping", err)
	}
	for _, r := range results {
		if r.Snippet != "" {
			return
		}
	}
	t.Error("expected at least one result with a snippet")
}

func mustNewIndex(t *testing.T) *Index {
	t.Helper()
	idx, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return idx
}

// TestWithCategoryBoosts verifies that WithCategoryBoosts records the
// boost map on the constructed Index. This is content-independent and
// always runs.
func TestWithCategoryBoosts(t *testing.T) {
	boosts := map[string]float64{
		"concepts": 2.0,
		"policies": 1.5,
	}
	idx, err := New(WithCategoryBoosts(boosts))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()

	if len(idx.categoryBoosts) != len(boosts) {
		t.Fatalf("categoryBoosts len = %d, want %d", len(idx.categoryBoosts), len(boosts))
	}
	for cat, want := range boosts {
		got, ok := idx.categoryBoosts[cat]
		if !ok {
			t.Errorf("categoryBoosts missing key %q", cat)
			continue
		}
		if got != want {
			t.Errorf("categoryBoosts[%q] = %v, want %v", cat, got, want)
		}
	}
}

// TestQuery_CategoryBoost: category-boosted queries surface pages from the
// boosted category first. The index is constructed with
// WithCategoryBoosts so the boost mechanism is exercised when content
// exists; individual cases skip when there are no hits (so the test
// stays dormant-but-correct on stripped builds).
func TestQuery_CategoryBoost(t *testing.T) {
	idx, err := New(WithCategoryBoosts(map[string]float64{"concepts": 2.0}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()

	// query → expected top hit ID prefix.
	cases := map[string]string{
		"callbacks":         "concepts/Callbacks",
		"rate limiting":     "concepts/Rate-Limiting",
		"circuit breaker":   "concepts/Circuit-Breaker",
		"incident response": "concepts/Incident-Response",
		"idempotency":       "concepts/Idempotency",
		"retries":           "concepts/Retries",
	}
	for q, wantID := range cases {
		t.Run(q, func(t *testing.T) {
			results, err := idx.Query(q, 3)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) == 0 {
				t.Skipf("no hits for %q in this snapshot", q)
			}
			if results[0].Page.ID != wantID {
				ids := make([]string, len(results))
				for i, r := range results {
					ids[i] = r.Page.ID
				}
				t.Errorf("query %q: top hit = %q, want %q. Top 3: %v",
					q, results[0].Page.ID, wantID, ids)
			}
		})
	}
}

// BenchmarkNew tracks index cold-start time. Useful regression
// target if we ever swap the search backend.
func BenchmarkNew(b *testing.B) {
	for i := 0; i < b.N; i++ {
		idx, err := New()
		if err != nil {
			b.Fatal(err)
		}
		idx.Close()
	}
}

// BenchmarkQuery measures per-query latency on a warm index.
func BenchmarkQuery(b *testing.B) {
	idx, err := New()
	if err != nil {
		b.Fatal(err)
	}
	defer idx.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := idx.Query("rate limiting retries", 10); err != nil {
			b.Fatal(err)
		}
	}
}
