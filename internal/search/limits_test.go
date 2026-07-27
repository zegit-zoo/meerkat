package search

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// TestValidateQuery covers the three pre-parse guards in isolation:
// overall length, paren nesting depth, and term count. Each case is
// sized so it trips exactly one guard (or none), so a regression in one
// check doesn't hide behind another.
func TestValidateQuery(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		wantErr bool
	}{
		{"empty", "", false}, // validateQuery isn't even reached for "" via QueryContext, but should still be a no-op
		{"short realistic query", "rate limiting", false},
		{"phrase query", `"circuit breaker"`, false},
		{"field targeted query", "title:eviction", false},
		{"boolean operators", "+retry -cache", false},
		{"or query", "cache OR queue", false},
		{"wildcard", "body:foo*", false},
		{"shallow parens", "(retry OR cache) AND timeout", false},
		{"at the length limit", strings.Repeat("a", maxQueryBytes), false},
		{"one byte over the length limit", strings.Repeat("a", maxQueryBytes+1), true},
		{"way over the length limit (mirrors the reported attack shape)", strings.Repeat("(", 50_000), true},
		{"at the paren depth limit", strings.Repeat("(", maxParenDepth), false},
		{"one paren over the depth limit", strings.Repeat("(", maxParenDepth+1), true},
		{"deep but short nesting", strings.Repeat("(", 20), true}, // 20 bytes: well under maxQueryBytes, isolates the depth guard
		{"balanced parens never accumulate depth", strings.Repeat("()", 100), false},
		{"at the term limit", strings.Repeat("a ", maxQueryTerms-1) + "a", false},
		{"one term over the limit", strings.Repeat("a ", maxQueryTerms+1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateQuery(tc.query)
			if tc.wantErr && err == nil {
				t.Fatalf("validateQuery(%.40q...): expected an error, got nil", tc.query)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateQuery(%.40q...): unexpected error: %v", tc.query, err)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidQuery) {
				t.Errorf("error %v does not wrap ErrInvalidQuery", err)
			}
		})
	}
}

// TestClampLimit exercises the limit-normalisation helper directly.
func TestClampLimit(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{-1, DefaultLimit},
		{0, DefaultLimit},
		{1, 1},
		{DefaultLimit, DefaultLimit},
		{MaxLimit, MaxLimit},
		{MaxLimit + 1, MaxLimit},
		{100_000, MaxLimit},
	}
	for _, tc := range cases {
		if got := clampLimit(tc.in); got != tc.want {
			t.Errorf("clampLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestQueryContext_RejectsOversizedQuery proves the guard runs inside
// the real Index.QueryContext path (not just validateQuery in
// isolation), and that the error is recognisable via errors.Is so HTTP
// and MCP can answer with a clean 400 / tool error.
func TestQueryContext_RejectsOversizedQuery(t *testing.T) {
	idx := newLimitsTestIndex(t)
	huge := strings.Repeat("x", maxQueryBytes+100)
	_, err := idx.QueryContext(context.Background(), huge, 10)
	if err == nil {
		t.Fatal("expected an error for an oversized query")
	}
	if !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("error %v does not wrap ErrInvalidQuery", err)
	}
}

// TestQueryContext_RejectsPathologicalNesting mirrors the confirmed
// attack shape (a long run of nested parens) at a size that's cheap to
// run in a test — the point is the guard fires, not re-measuring the
// vulnerability's exact cost.
func TestQueryContext_RejectsPathologicalNesting(t *testing.T) {
	idx := newLimitsTestIndex(t)
	nested := strings.Repeat("(", 100)
	_, err := idx.QueryContext(context.Background(), nested, 10)
	if err == nil {
		t.Fatal("expected an error for pathologically nested parens")
	}
	if !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("error %v does not wrap ErrInvalidQuery", err)
	}
}

// TestQueryContext_RejectsTooManyTerms isolates the clause-count guard.
func TestQueryContext_RejectsTooManyTerms(t *testing.T) {
	idx := newLimitsTestIndex(t)
	manyTerms := strings.Repeat("a ", maxQueryTerms+10)
	_, err := idx.QueryContext(context.Background(), manyTerms, 10)
	if err == nil {
		t.Fatal("expected an error for a query with too many terms")
	}
	if !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("error %v does not wrap ErrInvalidQuery", err)
	}
}

// TestQueryContext_NormalQueriesStillWork proves the new guards and the
// SearchInContext switch don't change results for ordinary queries:
// same fixtures and expectations as the pre-existing Query tests in
// inject_test.go, run through QueryContext with a real (non-expiring)
// context instead of Query's context.Background() wrapper.
func TestQueryContext_NormalQueriesStillWork(t *testing.T) {
	idx := newLimitsTestIndex(t)

	res, err := idx.QueryContext(context.Background(), "evicts", 10)
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected at least one hit for 'evicts'")
	}
	if res[0].Page.ID != "concepts/eviction" {
		t.Errorf("top hit = %q, want concepts/eviction", res[0].Page.ID)
	}

	// Query and QueryContext(context.Background(), ...) must agree.
	viaQuery, err := idx.Query("evicts", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(viaQuery) != len(res) || (len(res) > 0 && viaQuery[0].Page.ID != res[0].Page.ID) {
		t.Errorf("Query and QueryContext disagree: %+v vs %+v", viaQuery, res)
	}
}

// TestQueryContext_HonoursCancellation proves ctx really reaches
// bleve's search: an already-cancelled context must stop the query
// immediately with context.Canceled, not run to completion and return
// results. bleve's collector checks ctx.Done() once before scanning any
// document (see search/collector/topn.go's Collect), so this is
// deterministic regardless of corpus size.
func TestQueryContext_HonoursCancellation(t *testing.T) {
	idx := newLimitsTestIndex(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before QueryContext is ever called

	_, err := idx.QueryContext(ctx, "evicts", 10)
	if err == nil {
		t.Fatal("expected an error from an already-cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap context.Canceled", err)
	}
}

// TestQueryContext_HonoursDeadline is HonoursCancellation's sibling for
// an expired deadline instead of an explicit cancel, matching how
// internal/http.Config.QueryTimeout and internal/mcp's search handler
// bound a query.
func TestQueryContext_HonoursDeadline(t *testing.T) {
	idx := newLimitsTestIndex(t)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
	defer cancel()

	_, err := idx.QueryContext(ctx, "evicts", 10)
	if err == nil {
		t.Fatal("expected an error from an already-expired deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
	}
}

// TestQueryContext_CancelsMidFlight builds a corpus large enough that a
// full, uncancelled query measurably outlasts a short timeout, so this
// proves cancellation is honoured *during* collection (via
// collector.CheckDoneEvery), not merely before it starts. The timeout
// is generous relative to the expected full-scan time to keep this
// non-flaky on a slower CI machine, while still being far shorter than
// an uncancelled run.
func TestQueryContext_CancelsMidFlight(t *testing.T) {
	const numPages = 4000
	pages := make([]kb.Page, numPages)
	for i := range pages {
		pages[i] = fixturePage(
			pageID(i),
			"Widget document",
			"widget widget widget shared searchable term across every fixture page",
			"concepts",
		)
	}
	idx, err := NewFromPages(pages)
	if err != nil {
		t.Fatalf("NewFromPages: %v", err)
	}
	defer idx.Close()

	// Baseline: how long does an uncancelled run over this corpus take?
	start := time.Now()
	if _, err := idx.QueryContext(context.Background(), "widget shared searchable", numPages); err != nil {
		t.Fatalf("baseline QueryContext: %v", err)
	}
	baseline := time.Since(start)

	timeout := baseline / 4
	if timeout < time.Microsecond {
		t.Skipf("baseline query (%s) too fast on this machine to reliably measure mid-flight cancellation", baseline)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cancelStart := time.Now()
	_, err = idx.QueryContext(ctx, "widget shared searchable", numPages)
	elapsed := time.Since(cancelStart)

	if err == nil {
		t.Fatal("expected the timeout to cancel the in-flight query")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
	}
	// Generous upper bound: well under the uncancelled baseline, proving
	// this didn't just run to completion and incidentally also return an
	// error. Not a tight timing assertion — just enough to catch a
	// regression where ctx stops being threaded through.
	if elapsed > baseline {
		t.Errorf("cancelled query took %s, baseline (uncancelled) was %s — cancellation does not appear to have stopped it early", elapsed, baseline)
	}
}

func newLimitsTestIndex(t *testing.T) *Index {
	t.Helper()
	idx, err := NewFromPages([]kb.Page{
		fixturePage("concepts/eviction", "Cache eviction", "The cache evicts the least recently used entry.", "concepts"),
		fixturePage("concepts/sharding", "Sharding", "Data is split across shards by key hash.", "concepts"),
	})
	if err != nil {
		t.Fatalf("NewFromPages: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func pageID(i int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 0, 12)
	b = append(b, "fixture/"...)
	if i == 0 {
		b = append(b, letters[0])
	}
	for i > 0 {
		b = append(b, letters[i%len(letters)])
		i /= len(letters)
	}
	return string(b)
}
