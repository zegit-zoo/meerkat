// Package search builds an in-memory Bleve full-text index over the
// embedded knowledge base pages and exposes a small Query API.
//
// We deliberately use BM25 keyword search (no embeddings) because:
//   - The KB is small (~200 pages), so keyword matches are usually exact
//   - No external embedding model means the binary stays self-contained
//   - Cold-start is sub-second on every supported platform
//
// If we ever need semantic search, this is the place to swap in a
// different backend behind the same Index interface.
package search

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// Result is a single search hit with the matching page and a score.
type Result struct {
	Page    kb.Page
	Score   float64
	Snippet string // plain-text excerpt around the first match (best-effort)
}

// Index wraps a Bleve in-memory index over all KB pages.
type Index struct {
	bleve bleve.Index
	// mu guards pages. bleve's own index is already safe for concurrent
	// Index/Search calls; the page map beside it is not, and Put writes
	// to it while QueryContext reads — see Put.
	mu             sync.RWMutex
	pages          map[string]kb.Page
	categoryBoosts map[string]float64
}

// Option is a functional option for configuring an Index at construction time.
type Option func(*Index)

// WithCategoryBoosts configures a per-category boost map. When a document
// matches the query AND belongs to one of the given categories, an additional
// boosted conjunction clause is added to the disjunction — giving those
// category pages a scoring edge over the same query matched in other
// categories.
//
// The map key is the exact category value stored in frontmatter (matched
// as a keyword token, so case matters). The float64 value is the Bleve
// boost multiplier applied to the conjunction clause (title+body match AND
// category match).
//
// Example:
//
//	search.New(search.WithCategoryBoosts(map[string]float64{
//	    "concepts": 2.0,
//	    "policies": 1.5,
//	}))
//
// Default (no option supplied): empty map, no category-based boosting.
func WithCategoryBoosts(boosts map[string]float64) Option {
	return func(idx *Index) {
		idx.categoryBoosts = boosts
	}
}

// New builds a fresh in-memory index over every page in the embedded KB.
// It is safe to call multiple times; each call returns an independent
// index. Typical cold start: 50-100ms for ~200 pages.
//
// Functional options (e.g. WithCategoryBoosts) can be passed to customize
// ranking behavior. Callers that pass no options get a plain BM25 index
// with title/id/body field boosts and no category-based boosting.
func New(opts ...Option) (*Index, error) {
	pages, err := kb.List()
	if err != nil {
		return nil, fmt.Errorf("list pages: %w", err)
	}
	return NewFromPages(pages, opts...)
}

// NewFromPages builds an index over the supplied pages rather than the
// embedded KB. It is the injectable form of New: callers (and tests) that
// hold pages from a non-embedded source index them directly, without the
// build-time content embed. New is NewFromPages(kb.List(), opts...).
func NewFromPages(pages []kb.Page, opts ...Option) (*Index, error) {
	idx, err := bleve.NewMemOnly(buildMapping())
	if err != nil {
		return nil, fmt.Errorf("create bleve index: %w", err)
	}

	pageMap := make(map[string]kb.Page, len(pages))
	batch := idx.NewBatch()
	for _, p := range pages {
		if err := batch.Index(p.ID, indexDoc(p)); err != nil {
			return nil, fmt.Errorf("index %q: %w", p.ID, err)
		}
		pageMap[p.ID] = p
	}
	if err := idx.Batch(batch); err != nil {
		return nil, fmt.Errorf("apply batch: %w", err)
	}

	result := &Index{
		bleve:          idx,
		pages:          pageMap,
		categoryBoosts: make(map[string]float64),
	}
	for _, opt := range opts {
		opt(result)
	}
	return result, nil
}

// indexDoc is the document shape one page is indexed as. Shared by the
// bulk build and by Put, so an incrementally-indexed page cannot end up
// with different fields — or different boosts — from a page that was
// present at startup.
func indexDoc(p kb.Page) map[string]any {
	return map[string]any{
		"id":       p.ID,
		"title":    p.Title,
		"body":     p.Body,
		"category": p.Front.Category,
	}
}

// Put indexes (or re-indexes) a single page into a LIVE index, so it is
// searchable immediately — no rebuild, no restart.
//
// It exists for the memory toolset: mk_save_memory writes a document
// and the very next mk_search must find it, which is the whole point of
// saving a memory during a session. Bleve supports incremental
// Index(id, doc) against an open index, and re-indexing an existing ID
// replaces that document, so a save and a re-save of the same memory
// both do the right thing.
//
// Safe to call concurrently with searches: bleve serialises its own
// writes against readers, and the page map beside it is under mu.
func (i *Index) Put(p kb.Page) error {
	if p.ID == "" {
		return fmt.Errorf("cannot index a page with no ID")
	}
	if err := i.bleve.Index(p.ID, indexDoc(p)); err != nil {
		return fmt.Errorf("index %q: %w", p.ID, err)
	}
	i.mu.Lock()
	i.pages[p.ID] = p
	i.mu.Unlock()
	return nil
}

// page returns the stored page for a hit ID.
func (i *Index) page(id string) (kb.Page, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	p, ok := i.pages[id]
	return p, ok
}

// Query runs a free-text query and returns up to limit results, sorted
// by score (highest first). It is equivalent to
// QueryContext(context.Background(), q, limit) — kept as a thin wrapper
// for callers (the CLI, and existing tests) with no request-scoped
// context to thread through. Prefer QueryContext for anything reachable
// by a remote caller: see its doc comment for why the context matters.
func (i *Index) Query(q string, limit int) ([]Result, error) {
	return i.QueryContext(context.Background(), q, limit)
}

// QueryContext is Query with an explicit context. ctx is threaded into
// bleve's SearchInContext, so a client disconnect or a caller-imposed
// deadline (e.g. internal/http.Config.QueryTimeout, or the timeout
// internal/mcp's search handler applies) actually stops the underlying
// search: bleve's collector checks ctx.Done() every
// collector.CheckDoneEvery (1024) documents and returns ctx.Err()
// immediately when it fires, instead of running to completion after
// the caller has stopped waiting.
//
// That only bounds the search/collection phase, though. The raw query
// string q is validated BEFORE any of that — see validateQuery — since
// bleve's query-string parser can burn substantial CPU on pathological
// input synchronously, with no context check anywhere in that code
// path (see limits.go's doc comment for the mechanism). limit is
// clamped via clampLimit: non-positive becomes DefaultLimit, and
// anything above MaxLimit is capped there.
//
// Bleve handles tokenisation, stemming, and BM25 ranking. Empty queries
// return no results without erroring.
//
// Field boosts (multiplied with BM25 score):
//
//	title  × 5.0
//	id     × 3.0
//	body   × 1.0   (baseline)
//
// Category boosts (configurable via WithCategoryBoosts):
//
//	For each category→weight entry, a conjunction clause (category match
//	AND content match) is added to the disjunction with the given weight.
//	This gives pages in boosted categories an edge for matching queries
//	while still allowing non-boosted pages to surface via the
//	title/id/body clauses. If no category boosts are configured (the
//	default), the query is a plain title/id/body disjunction.
func (i *Index) QueryContext(ctx context.Context, q string, limit int) ([]Result, error) {
	if q == "" {
		return nil, nil
	}
	if err := validateQuery(q); err != nil {
		return nil, err
	}
	limit = clampLimit(limit)

	// Build a disjunction across (boosted title) OR (boosted id) OR
	// (body) OR (per-category boost clauses). The body fallback lets us
	// still match on free-text terms while strongly preferring documents
	// whose title or path matches. Category boost clauses only fire when
	// the doc is in the relevant category AND another clause matched.
	titleQ := bleve.NewMatchQuery(q)
	titleQ.SetField("title")
	titleQ.SetBoost(5.0)

	idQ := bleve.NewMatchQuery(q)
	idQ.SetField("id")
	idQ.SetBoost(3.0)

	// Body uses query string so phrase/syntax features still work
	// for free-text queries.
	bodyQ := bleve.NewQueryStringQuery(q)

	clauses := []query.Query{titleQ, idQ, bodyQ}

	// For each configured category→weight, add a conjunction clause that
	// fires only when the document belongs to that category AND matches
	// the query content. This keeps non-boosted categories scoring
	// naturally via the title/id/body clauses above.
	for category, weight := range i.categoryBoosts {
		catQ := bleve.NewTermQuery(category)
		catQ.SetField("category")

		contentQ := bleve.NewQueryStringQuery(q)

		catBoost := bleve.NewConjunctionQuery(catQ, contentQ)
		catBoost.SetBoost(weight)
		clauses = append(clauses, catBoost)
	}

	combined := bleve.NewDisjunctionQuery(clauses...)
	req := bleve.NewSearchRequestOptions(combined, limit, 0, false)
	req.Highlight = bleve.NewHighlight()
	req.Highlight.AddField("body")

	res, err := i.bleve.SearchInContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("search %q: %w", q, err)
	}

	out := make([]Result, 0, len(res.Hits))
	for _, hit := range res.Hits {
		page, ok := i.page(hit.ID)
		if !ok {
			continue // index/page map drift - shouldn't happen
		}
		snippet := ""
		if frags, ok := hit.Fragments["body"]; ok && len(frags) > 0 {
			snippet = frags[0]
		}
		out = append(out, Result{
			Page:    page,
			Score:   hit.Score,
			Snippet: snippet,
		})
	}
	// Bleve already sorts by score, but be explicit so callers can
	// trust the contract regardless of backend.
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out, nil
}

// Close releases resources held by the index. Cheap for in-memory
// indexes but kept for symmetry with the persistent variant we may add
// later.
func (i *Index) Close() error {
	if i.bleve == nil {
		return nil
	}
	return i.bleve.Close()
}

// buildMapping configures Bleve's analysis pipeline. Title gets a
// higher boost than body so page-name matches outrank casual mentions.
func buildMapping() *mapping.IndexMappingImpl {
	im := bleve.NewIndexMapping()

	docMap := bleve.NewDocumentMapping()

	titleField := bleve.NewTextFieldMapping()
	titleField.Analyzer = "standard"
	docMap.AddFieldMappingsAt("title", titleField)

	idField := bleve.NewTextFieldMapping()
	idField.Analyzer = "standard"
	docMap.AddFieldMappingsAt("id", idField)

	bodyField := bleve.NewTextFieldMapping()
	bodyField.Analyzer = "standard"
	bodyField.Store = true // needed for Highlight
	docMap.AddFieldMappingsAt("body", bodyField)

	// category is indexed as a single keyword token (no analysis) so
	// values match exactly, not as stemmed/analysed tokens.
	categoryField := bleve.NewTextFieldMapping()
	categoryField.Analyzer = "keyword"
	docMap.AddFieldMappingsAt("category", categoryField)

	im.AddDocumentMapping("_default", docMap)
	return im
}
