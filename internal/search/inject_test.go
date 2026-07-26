package search

import (
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// These tests exercise the real indexing + ranking logic against
// hand-built pages via NewFromPages, so they run regardless of whether
// any content is embedded at build time (the embedded KB may be stripped).

func fixturePage(id, title, body, category string) kb.Page {
	return kb.Page{
		ID:    id,
		Title: title,
		Body:  body,
		Front: kb.Frontmatter{ID: id, Title: title, Category: category},
	}
}

func newTestIndex(t *testing.T, pages []kb.Page, opts ...Option) *Index {
	t.Helper()
	idx, err := NewFromPages(pages, opts...)
	if err != nil {
		t.Fatalf("NewFromPages: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestNewFromPages_FindsTermInBody(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{
		fixturePage("concepts/eviction", "Cache eviction", "The cache evicts the least recently used entry.", "concepts"),
		fixturePage("concepts/sharding", "Sharding", "Data is split across shards by key hash.", "concepts"),
	})
	res, err := idx.Query("evicts", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected at least one hit for 'evicts'")
	}
	if res[0].Page.ID != "concepts/eviction" {
		t.Errorf("top hit = %q, want concepts/eviction", res[0].Page.ID)
	}
}

func TestNewFromPages_TitleOutranksBody(t *testing.T) {
	// Both pages mention "kangaroo"; one in the title (boost 5.0), one
	// only in the body. The title match must rank first.
	idx := newTestIndex(t, []kb.Page{
		fixturePage("a/body-only", "Marsupials", "A kangaroo hops across the outback.", "concepts"),
		fixturePage("b/title", "Kangaroo behaviour", "These animals are social grazers.", "concepts"),
	})
	res, err := idx.Query("kangaroo", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) < 2 {
		t.Fatalf("expected both pages to match, got %d", len(res))
	}
	if res[0].Page.ID != "b/title" {
		t.Errorf("title match should rank first; got %q", res[0].Page.ID)
	}
	if !(res[0].Score > res[1].Score) {
		t.Errorf("expected top score %.3f > next %.3f", res[0].Score, res[1].Score)
	}
}

func TestNewFromPages_CategoryBoostReordersTies(t *testing.T) {
	pages := []kb.Page{
		fixturePage("policies/retention", "Retention", "The retention window is thirty days.", "policies"),
		fixturePage("concepts/retention", "Retention", "The retention window is thirty days.", "concepts"),
	}
	// Without a boost, the two equally-matching pages tie; with a strong
	// concepts boost, the concepts page must come first.
	idx := newTestIndex(t, pages, WithCategoryBoosts(map[string]float64{"concepts": 10.0}))
	res, err := idx.Query("retention window", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) < 2 {
		t.Fatalf("expected both pages, got %d", len(res))
	}
	if res[0].Page.Front.Category != "concepts" {
		t.Errorf("boosted category should rank first; got %q", res[0].Page.Front.Category)
	}
}

func TestNewFromPages_Snippet(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{
		fixturePage("concepts/idempotency", "Idempotency", "An idempotent operation can be retried safely without side effects.", "concepts"),
	})
	res, err := idx.Query("idempotent", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(res))
	}
	if res[0].Snippet == "" {
		t.Error("expected a non-empty snippet for a body match")
	}
}

func TestNewFromPages_LimitClamping(t *testing.T) {
	pages := []kb.Page{
		fixturePage("p/1", "Alpha signal", "shared term signal", "concepts"),
		fixturePage("p/2", "Beta signal", "shared term signal", "concepts"),
		fixturePage("p/3", "Gamma signal", "shared term signal", "concepts"),
	}
	idx := newTestIndex(t, pages)
	res, err := idx.Query("signal", 2)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) != 2 {
		t.Errorf("limit=2 should cap results at 2, got %d", len(res))
	}
}

func TestNewFromPages_EmptyQueryReturnsNil(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{fixturePage("p/1", "T", "body", "concepts")})
	res, err := idx.Query("", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res != nil {
		t.Errorf("empty query should return nil, got %d results", len(res))
	}
}

func TestNewFromPages_NoMatch(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{fixturePage("p/1", "Title", "completely unrelated body", "concepts")})
	res, err := idx.Query("nonexistentterm", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("expected no hits, got %d", len(res))
	}
}

func TestNewFromPages_EmptyIndex(t *testing.T) {
	idx := newTestIndex(t, nil)
	res, err := idx.Query("anything", 10)
	if err != nil {
		t.Fatalf("Query on empty index: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("empty index should yield no hits, got %d", len(res))
	}
}

func TestIndex_CloseIdempotentOnNil(t *testing.T) {
	// A zero Index (no bleve handle) closes cleanly.
	var i Index
	if err := i.Close(); err != nil {
		t.Errorf("Close on nil bleve: %v", err)
	}
}

func TestResultSnippetHighlightsTerm(t *testing.T) {
	idx := newTestIndex(t, []kb.Page{
		fixturePage("concepts/quorum", "Quorum", "A write succeeds once a quorum of replicas acknowledges it.", "concepts"),
	})
	res, err := idx.Query("quorum", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(res))
	}
	// Bleve wraps the matched term in <mark> tags by default.
	if !strings.Contains(strings.ToLower(res[0].Snippet), "quorum") {
		t.Errorf("snippet should contain the matched term; got %q", res[0].Snippet)
	}
}
