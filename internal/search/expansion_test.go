package search

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/blevesearch/bleve/v2/search/searcher"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// expansionPrefixes are the three-letter stems every word of the
// expansion corpus starts with. Few stems over many words make each stem
// match thousands of dictionary terms, which is what makes a wildcard,
// regexp or prefix clause expensive.
var expansionPrefixes = []string{
	"abc", "bcd", "cde", "def", "efg", "fgh", "ghi", "hij", "ijk", "jkl",
	"klm", "lmn", "mno", "nop", "opq", "pqr", "qrs", "rst", "stu", "tuv",
}

// expansionCorpus builds n small pages over a large vocabulary: every
// word is a stem from expansionPrefixes plus a random four-letter
// suffix, so the body dictionary holds on the order of n*40 distinct
// terms and each stem prefixes thousands of them. Deterministic (fixed
// seed) so the bounds below are reproducible.
func expansionCorpus(n int) []kb.Page {
	r := rand.New(rand.NewPCG(32, 63))
	word := func() string {
		var b strings.Builder
		b.WriteString(expansionPrefixes[r.IntN(len(expansionPrefixes))])
		for range 4 {
			b.WriteByte(byte('a' + r.IntN(26)))
		}
		return b.String()
	}
	pages := make([]kb.Page, 0, n)
	for i := range n {
		var body strings.Builder
		for range 40 {
			body.WriteString(word())
			body.WriteByte(' ')
		}
		id := fmt.Sprintf("bulk/page-%05d", i)
		pages = append(pages, fixturePage(id, word()+" "+word(), body.String(), "concepts"))
	}
	return pages
}

// padTo512 repeats unit until one more copy would pass maxQueryBytes or
// maxQueryTerms, so every hostile query is as large as the shape guard
// allows.
func padTo512(unit string) string {
	var b strings.Builder
	terms := 0
	for b.Len()+len(unit) <= maxQueryBytes && terms+len(strings.Fields(unit)) <= maxQueryTerms {
		b.WriteString(unit)
		terms += len(strings.Fields(unit))
	}
	return b.String()
}

// TestQuery_ExpansionIsBounded is the regression test for unbounded
// multi-term expansion: on a few thousand pages with a large vocabulary,
// no maximal-size query built from wildcards, regexps, prefixes or fuzzy
// terms may run long or allocate much, whether it is rejected up front,
// rejected as too broad, or answered. The bounds are generous (several
// times what was measured) so they fail on a regression, not on a slow
// CI runner; without the limits the first two queries alone took tens of
// seconds and gigabytes on a corpus this size.
func TestQuery_ExpansionIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 3,000-page index; skipped under -short")
	}
	const (
		maxElapsed = 3 * time.Second
		maxAlloc   = 256 << 20 // bytes allocated by one query
	)
	pages := expansionCorpus(3000)
	idx := newTestIndex(t, pages, WithCategoryBoosts(map[string]float64{"concepts": 2}))

	cases := []struct {
		name  string
		query string
	}{
		{"match-everything regexps", padTo512("/.*/ ")},
		{"single-letter wildcards", padTo512("a* ")},
		{"leading wildcards", padTo512("*bc ")},
		{"stem wildcards", padTo512("abc* ")},
		{"two broad wildcards", "abc* bcd*"},
		{"two broad regexps", "/abc.*/ /bcd.*/"},
		{"field-targeted broad wildcards", "body:abc* title:bcd*"},
		{"bare stems fall to the prefix stage", padTo512("abc bcd cde def ")},
		{"maximal fuzziness", padTo512("abcdefgh~2 ")},
		{"fuzzy-stage misses", padTo512("zyxwvuts ")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.query) > maxQueryBytes {
				t.Fatalf("test query is %d bytes, over the %d limit", len(tc.query), maxQueryBytes)
			}
			ctx, cancel := context.WithTimeout(context.Background(), DefaultQueryTimeout)
			defer cancel()

			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			_, err := idx.QueryContext(ctx, tc.query, 10)
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)
			alloc := after.TotalAlloc - before.TotalAlloc

			if err != nil && !errors.Is(err, ErrInvalidQuery) {
				t.Fatalf("QueryContext: %v (want a result or ErrInvalidQuery)", err)
			}
			t.Logf("%d bytes: %s, %d KiB allocated, err=%v", len(tc.query), elapsed, alloc>>10, err)
			if elapsed > maxElapsed {
				t.Errorf("query took %s, over the %s bound", elapsed, maxElapsed)
			}
			if alloc > maxAlloc {
				t.Errorf("query allocated %d MiB, over the %d MiB bound", alloc>>20, maxAlloc>>20)
			}
		})
	}
}

// TestQuery_TooBroadExpansionIsInvalidQuery pins tooBroad's match on
// bleve's error text: a trailing wildcard on a stem that prefixes
// thousands of terms is valid syntax, so it reaches bleve, and must come
// back as ErrInvalidQuery rather than an internal error.
func TestQuery_TooBroadExpansionIsInvalidQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 3,000-page index; skipped under -short")
	}
	idx := newTestIndex(t, expansionCorpus(3000))
	for _, q := range []string{"abc*", "/abc.*/", "abc"} {
		_, err := idx.QueryContext(context.Background(), q, 10)
		if !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%q: err = %v, want ErrInvalidQuery", q, err)
		}
		if err != nil && !strings.Contains(err.Error(), "more specific") {
			t.Errorf("%q: error %q does not tell the caller what to do", q, err)
		}
	}
}

func TestExpansionLimitIsInstalled(t *testing.T) {
	if searcher.DisjunctionMaxClauseCount != maxTermExpansion {
		t.Fatalf("searcher.DisjunctionMaxClauseCount = %d, want %d", searcher.DisjunctionMaxClauseCount, maxTermExpansion)
	}
}

// TestValidateSyntax covers the parsed-query checks: legitimate syntax
// passes, and every multi-term shape the limits exist for is refused
// with ErrInvalidQuery.
func TestValidateSyntax(t *testing.T) {
	cases := []struct {
		query   string
		wantErr bool
	}{
		{"rate limiting", false},
		{`"circuit breaker"`, false},
		{"+retry -cache", false},
		{"title:eviction", false},
		{"kube*", false},
		{"body:foo*", false},
		{"helm* flux*", false},
		{"fo?bar", false},
		{"/kube.*/", false},
		{"datadgo~1 monitr~2", false},
		{"(retry OR cache) AND timeout", false},
		{"*", true},
		{"*netes", true},
		{"?ube", true},
		{"body:*foo", true},
		{"/.*/", true},
		{"/(?i)kube/", true},
		{"/[a-z]+/", true},
		{"/a|.*/", true},
		{"helm* flux* argo*", true},
		{"/abc.*/ /def.*/ ghi*", true},
		{"+a* -b* c*", true},
		{"/(unclosed/", true},
		{"title:", true},
	}
	for _, tc := range cases {
		err := validateSyntax(tc.query)
		if tc.wantErr && !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("validateSyntax(%q) = %v, want ErrInvalidQuery", tc.query, err)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("validateSyntax(%q) = %v, want nil", tc.query, err)
		}
	}
}

// TestQuery_LegitimateMultiTermSyntaxStillWorks proves the limits leave
// the documented syntax working end to end: a trailing wildcard, a
// field-targeted wildcard, a regexp with a literal prefix and a phrase.
func TestQuery_LegitimateMultiTermSyntaxStillWorks(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{
		fixturePage("ops/kubernetes", "Kubernetes upgrades", "Upgrading kubernetes clusters with zero downtime.", "concepts"),
		fixturePage("ops/helm", "Helm releases", "A helmrelease pins a chart version.", "concepts"),
	})
	for q, want := range map[string]string{
		"kube*":           "ops/kubernetes",
		"body:helmrel*":   "ops/helm",
		"/kuber.*/":       "ops/kubernetes",
		`"zero downtime"`: "ops/kubernetes",
		"helm* upgrad*":   "",
	} {
		res, err := idx.QueryContext(context.Background(), q, 10)
		if err != nil {
			t.Errorf("%q: %v", q, err)
			continue
		}
		if len(res) == 0 {
			t.Errorf("%q: no results", q)
			continue
		}
		if want != "" && res[0].Page.ID != want {
			t.Errorf("%q: top hit %q, want %q", q, res[0].Page.ID, want)
		}
	}
}

// TestSearch_WaitsForASlot proves searches share the process-wide slots:
// with every slot taken, a query waits, and gives up with the caller's
// deadline instead of running.
func TestSearch_WaitsForASlot(t *testing.T) {
	idx := newLimitsTestIndex(t)
	for range maxConcurrentSearches {
		if err := acquireSearchSlot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		for range maxConcurrentSearches {
			releaseSearchSlot()
		}
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := idx.QueryContext(ctx, "evicts", 10)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded while every slot is taken", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("gave up after %s, want about the 50ms deadline", elapsed)
	}

	release()
	if res, err := idx.QueryContext(context.Background(), "evicts", 10); err != nil || len(res) == 0 {
		t.Fatalf("after release: %v, %d results", err, len(res))
	}
}

// TestSearch_ReturnsAtTheDeadlineAndKeepsTheSlot proves the caller is
// released at its deadline even while bleve is still working, and that
// the abandoned search keeps its slot until bleve actually returns. The
// stall is made deterministic by holding the page-map lock the type-boost
// score callback needs, which parks bleve mid-search.
func TestSearch_ReturnsAtTheDeadlineAndKeepsTheSlot(t *testing.T) {
	idx := newLimitsTestIndex(t)
	if !idx.boostsReorder() {
		t.Fatal("fixture needs type boosts, so bleve calls back into the index while scoring")
	}
	// Make one page carry a boosted type so the callback runs.
	idx.mu.Lock()
	p := idx.pages["concepts/eviction"]
	p.Front.Type = kb.TypePointer
	idx.pages["concepts/eviction"] = p
	// Keep holding mu: the score callback (boostedScore → page) blocks.

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := idx.QueryContext(ctx, "evicts", 10)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		idx.mu.Unlock()
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("returned after %s, want about the 100ms deadline", elapsed)
	}
	if n := len(searchSlots); n != 1 {
		t.Errorf("%d slots held after the caller gave up, want 1 (the abandoned search's)", n)
	}

	idx.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for len(searchSlots) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the abandoned search never released its slot")
		}
		time.Sleep(time.Millisecond)
	}
}
