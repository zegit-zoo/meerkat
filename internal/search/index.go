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
	"math"
	"sort"
	"sync"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/token/edgengram"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"

	"github.com/zegit-zoo/meerkat/internal/kb"
)

// Result is a single search hit with the matching page and a score.
type Result struct {
	Page    kb.Page
	Score   float64
	Snippet string // plain-text excerpt around the first match (best-effort)
	// Stage is the planner stage that produced this result: exact,
	// fuzzy or prefix. See planner.go.
	Stage Stage
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
	titleAnalyzer  string
	typeBoosts     map[string]float64
}

// Title analyzers a collection may choose (Layout.Analyzer in
// content-source.yaml).
const (
	// AnalyzerStandard is bleve's standard analyzer: unicode
	// tokenisation, lowercase, English stop words. The default.
	AnalyzerStandard = "standard"
	// AnalyzerNgram indexes titles as edge n-grams (3–8 characters) so
	// a partial or misspelt word still matches its beginning
	// ("datadg" → "datadog"). It multiplies the title term count and is
	// meant for a hub tier: a small collection of routing pages. It is
	// refused for a corpus over MaxNgramCorpusBytes.
	AnalyzerNgram = "ngram"
	// ngramAnalyzerName is the registered custom analyzer.
	ngramAnalyzerName = "meerkat_title_ngram"
)

// MaxNgramCorpusBytes bounds the corpus an n-gram title analyzer may be
// used on: 1 MiB, the hub tier size the mob design sets.
const MaxNgramCorpusBytes = 1 << 20

// WithTitleAnalyzer selects the analyzer for the title field:
// AnalyzerStandard (default) or AnalyzerNgram.
func WithTitleAnalyzer(name string) Option {
	return func(idx *Index) {
		idx.titleAnalyzer = name
	}
}

// DefaultTypeBoosts is what an index ranks by when WithTypeBoosts is not
// given: a hub's routing pages outrank its own thin content, and the
// two other members of a capability bundle sit between a pointer and a
// plain page. The values are starting points to be tuned from retrieval
// telemetry (meerkat-mob issue F), not constants of nature.
var DefaultTypeBoosts = map[string]float64{
	kb.TypePointer: 4.0,
	kb.TypeSkill:   2.0,
	kb.TypeExample: 1.5,
}

// WithTypeBoosts configures a per-`type` boost map: a hit whose
// frontmatter type is in the map has its final score multiplied by the
// weight (see QueryAs for why this is a multiplier where category
// boosts are a clause). Passing an empty map disables type boosting;
// not passing the option at all applies DefaultTypeBoosts.
func WithTypeBoosts(boosts map[string]float64) Option {
	return func(idx *Index) {
		idx.typeBoosts = boosts
	}
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
	// Options that shape the mapping must be known before the index
	// exists; apply them to a scratch value first.
	var pre Index
	for _, opt := range opts {
		opt(&pre)
	}
	switch pre.titleAnalyzer {
	case "", AnalyzerStandard:
		pre.titleAnalyzer = AnalyzerStandard
	case AnalyzerNgram:
		var size int
		for _, p := range pages {
			size += len(p.Title) + len(p.Body)
		}
		if size > MaxNgramCorpusBytes {
			return nil, fmt.Errorf("title analyzer %q is for a hub tier: this corpus is %d bytes, over the %d-byte cap", AnalyzerNgram, size, MaxNgramCorpusBytes)
		}
	default:
		return nil, fmt.Errorf("unknown title analyzer %q (want %s or %s)", pre.titleAnalyzer, AnalyzerStandard, AnalyzerNgram)
	}
	mapping, err := buildMapping(pre.titleAnalyzer)
	if err != nil {
		return nil, err
	}
	idx, err := bleve.NewMemOnly(mapping)
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
		titleAnalyzer:  pre.titleAnalyzer,
		typeBoosts:     DefaultTypeBoosts,
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
		"id":             p.ID,
		"title":          p.Title,
		"body":           p.Body,
		"category":       p.Front.Category,
		subcategoryField: p.Front.Subcategory,
		statusField:      p.Front.Status,
		typeField:        p.Front.Type,
		tagsField:        p.Front.Tags,
		ownerField:       ownerToken(p),
	}
}

// The visibility field, and the two token shapes it holds.
//
// EVERY document carries one, including public ones: a document with no
// owner field could not be matched by a term query, so "visible to
// everyone" needs a token of its own rather than an absence. publicToken
// is a value no owner can collide with — internal/memory's namespaces
// are a slug plus a 16-hex digest, or the fixed "local" — and the
// private form is prefixed anyway, so the two spaces cannot overlap even
// if that ever changed.
// Keyword fields indexed from frontmatter (meerkat-mob issue C) so
// mk_list's filters can be query clauses instead of a post-list walk.
// Each is a keyword: the exact value, never a token of it.
const (
	subcategoryField = "subcategory"
	statusField      = "status"
	typeField        = "type"
	tagsField        = "tags"
)

const (
	ownerField  = "owner"
	publicToken = "public"
	ownerPrefix = "own:"
	// ownerBoost is zero so the visibility clause decides eligibility
	// without touching the score. See visibilityClause.
	ownerBoost = 0.0
)

// keywordAnalyzer indexes a field as one verbatim token: no splitting,
// no stemming, no lowercasing. Used for the two fields whose values are
// identifiers to be matched exactly rather than prose to be searched.
const keywordAnalyzer = "keyword"

// ownerToken is the value a page's visibility field is indexed as.
func ownerToken(p kb.Page) string { return ownerTokenFor(p.PrivateOwner()) }

// ownerTokenFor maps a derived owner to its indexed token.
func ownerTokenFor(owner string) string {
	if owner == "" {
		return publicToken
	}
	return ownerPrefix + owner
}

// visibilityClause is the MANDATORY query clause a restricted viewer's
// search is conjoined with, and it is the whole reason per-page
// visibility is enforceable at all.
//
// It has to be part of the query rather than a filter over the results,
// because the two are not equivalent: bleve ranks and truncates to
// `limit` inside SearchInContext, so a caller whose top ten hits are
// somebody else's private memories would get an empty page of results
// from a post-filter, while the same query with the clause in it returns
// the ten best documents that caller is actually allowed to see.
// Filtering afterwards also has to have loaded the metadata it then
// discards, which is how snippets and counts leak.
//
// The clause is boosted to zero so it contributes nothing to the score:
// a conjunction's score is the sum of its children's, and a zero query
// boost makes both the term's weight and its score zero, leaving the
// content clauses' BM25 scores exactly as they were. Visibility decides
// what is eligible; it does not decide what ranks.
//
// An unfiltered viewer gets no clause at all, so the query executed for
// the CLI and for a single-user deployment is byte-for-byte the one that
// ran before per-page visibility existed.
func visibilityClause(v kb.Viewer) query.Query {
	if v.IsUnfiltered() {
		return nil
	}
	public := bleve.NewTermQuery(publicToken)
	public.SetField(ownerField)
	public.SetBoost(ownerBoost)
	if v.Owner() == "" {
		return public
	}
	own := bleve.NewTermQuery(ownerTokenFor(v.Owner()))
	own.SetField(ownerField)
	own.SetBoost(ownerBoost)
	either := bleve.NewDisjunctionQuery(public, own)
	either.SetBoost(ownerBoost)
	return either
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

// QueryContext is QueryAs for a viewer that sees every page. It is what
// a single-user surface (the CLI, `mk http serve`) wants: there is one
// principal in front of it and they own everything.
//
// Anything serving several principals at once must call QueryAs with
// that caller's viewer instead. Nothing here can tell the difference, so
// the enforcement point is one layer up: collections.Registry.Search
// always passes the viewer its per-request view was built with, and the
// MCP surface only ever reaches an index through that.
func (i *Index) QueryContext(ctx context.Context, q string, limit int) ([]Result, error) {
	return i.QueryAs(ctx, kb.Unfiltered(), q, limit)
}

// QueryAs is Query with an explicit context and an explicit VIEWER. ctx is threaded into
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
//
// Visibility (see visibilityClause):
//
//	The whole disjunction above is conjoined with a zero-boosted
//	visibility clause matching the pages v may see, so ineligible
//	documents are excluded BEFORE bleve ranks and truncates to limit —
//	not filtered out of the results afterwards, which would leak
//	metadata and silently shrink the result set. An unfiltered viewer
//	adds no clause, and scores are unchanged either way.
func (i *Index) QueryAs(ctx context.Context, v kb.Viewer, q string, limit int) ([]Result, error) {
	out, _, err := i.QueryStaged(ctx, v, q, limit)
	return out, err
}

// exactQuery builds the exact stage: the BM25 query meerkat has always
// run — title (×5) and id (×3) match queries plus the bleve query-string
// form over the body, plus one boosted clause per configured category.
func (i *Index) exactQuery(q string) query.Query {
	titleQ := bleve.NewMatchQuery(q)
	titleQ.SetField("title")
	titleQ.SetBoost(5.0)

	idQ := bleve.NewMatchQuery(q)
	idQ.SetField("id")
	idQ.SetBoost(3.0)

	bodyQ := bleve.NewQueryStringQuery(q)

	clauses := make([]query.Query, 0, 3+len(i.categoryBoosts))
	clauses = append(clauses, titleQ, idQ, bodyQ)

	for category, weight := range i.categoryBoosts {
		catQ := bleve.NewTermQuery(category)
		catQ.SetField("category")

		contentQ := bleve.NewQueryStringQuery(q)

		catBoost := bleve.NewConjunctionQuery(catQ, contentQ)
		catBoost.SetBoost(weight)
		clauses = append(clauses, catBoost)
	}
	return bleve.NewDisjunctionQuery(clauses...)
}

// run executes one stage's query under the viewer's visibility clause
// and assembles results with snippets.
func (i *Index) run(ctx context.Context, v kb.Viewer, combined query.Query, limit int) ([]Result, error) {
	if vis := visibilityClause(v); vis != nil {
		combined = bleve.NewConjunctionQuery(vis, combined)
	}
	req := bleve.NewSearchRequestOptions(combined, limit, 0, false)
	req.Highlight = bleve.NewHighlight()
	req.Highlight.AddField("body")

	res, err := i.bleve.SearchInContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	out := make([]Result, 0, len(res.Hits))
	for _, hit := range res.Hits {
		page, ok := i.page(hit.ID)
		if !ok {
			continue // index/page map drift - shouldn't happen
		}
		if !v.CanSee(page) {
			continue
		}
		snippet := ""
		if frags, ok := hit.Fragments["body"]; ok && len(frags) > 0 {
			snippet = frags[0]
		}
		score := hit.Score
		// Type boosts multiply the FINAL score rather than adding a
		// clause the way category boosts do: an additive clause cannot
		// reliably lift a two-line pointer page above a content page
		// whose title matches every term, and "the hub's routing pages
		// outrank its own content" is the property the mob design
		// needs. A multiplier makes the rule legible: a typed page wins
		// whenever its own match is within 1/boost of the best content
		// hit.
		if weight, ok := i.typeBoosts[page.Front.Type]; ok && weight > 0 {
			score *= weight
		}
		out = append(out, Result{
			Page:    page,
			Score:   score,
			Snippet: snippet,
		})
	}
	// Bleve already sorts by score, but be explicit so callers can
	// rely on the order.
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out, nil
}

// Close releases resources held by the index. Cheap for in-memory
// indexes but kept for symmetry with the persistent variant we may add
// later.
// SizeEstimate returns an estimate of the bytes this index and its
// pages hold in memory: the page bytes (title + body) plus what bleve
// reports for its in-memory segments, or twice the page bytes when it
// reports nothing. An estimate — the cache budget (meerkat-mob issue
// E) needs a number that moves with content size, not an exact one.
func (i *Index) SizeEstimate() int64 {
	i.mu.RLock()
	var pages int64
	for _, p := range i.pages {
		pages += int64(len(p.Title) + len(p.Body))
	}
	i.mu.RUnlock()
	var index int64
	if i.bleve != nil {
		if m, ok := i.bleve.StatsMap()["index"].(map[string]any); ok {
			if n, ok := m["num_bytes_used_disk"].(uint64); ok && n <= math.MaxInt64 {
				index = int64(n)
			}
		}
	}
	if index <= 0 {
		index = 2 * pages
	}
	return pages + index
}

func (i *Index) Close() error {
	if i.bleve == nil {
		return nil
	}
	return i.bleve.Close()
}

// buildMapping configures Bleve's analysis pipeline. Title gets a
// higher boost than body so page-name matches outrank casual mentions.
func buildMapping(titleAnalyzer string) (*mapping.IndexMappingImpl, error) {
	im := bleve.NewIndexMapping()

	docMap := bleve.NewDocumentMapping()

	titleField := bleve.NewTextFieldMapping()
	titleField.Analyzer = "standard"
	if titleAnalyzer == AnalyzerNgram {
		if err := registerNgramAnalyzer(im); err != nil {
			return nil, err
		}
		titleField.Analyzer = ngramAnalyzerName
	}
	docMap.AddFieldMappingsAt("title", titleField)

	for _, f := range []string{subcategoryField, statusField, typeField, tagsField} {
		kw := bleve.NewTextFieldMapping()
		kw.Analyzer = keywordAnalyzer
		kw.IncludeInAll = false
		kw.IncludeTermVectors = false
		docMap.AddFieldMappingsAt(f, kw)
	}

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
	categoryField.Analyzer = keywordAnalyzer
	docMap.AddFieldMappingsAt("category", categoryField)

	// owner carries per-page visibility (see visibilityClause). Keyword
	// analysis is load-bearing rather than a preference: the standard
	// analyzer would split "own:alice-8f2a1c0b9d8e7f60" on the colon and
	// the dash and stem the pieces, so one owner's token could match
	// another's. One verbatim token per document means a term query
	// matches exactly one owner and no other.
	//
	// IncludeInAll is off so the token never becomes part of the _all
	// field a free-text query can reach: a caller must not be able to
	// search for "own:<somebody-else>" and learn from the hit count that
	// the somebody exists.
	ownerFieldMapping := bleve.NewTextFieldMapping()
	ownerFieldMapping.Analyzer = keywordAnalyzer
	ownerFieldMapping.IncludeInAll = false
	ownerFieldMapping.IncludeTermVectors = false
	docMap.AddFieldMappingsAt(ownerField, ownerFieldMapping)

	im.AddDocumentMapping("_default", docMap)
	return im, nil
}

// registerNgramAnalyzer adds the edge n-gram title analyzer to a
// mapping: unicode tokens, lowercased, expanded to their 3–8 character
// prefixes.
func registerNgramAnalyzer(im *mapping.IndexMappingImpl) error {
	if err := im.AddCustomTokenFilter("meerkat_edge_ngram", map[string]any{
		"type": edgengram.Name,
		"min":  3.0,
		"max":  8.0,
	}); err != nil {
		return fmt.Errorf("register edge n-gram filter: %w", err)
	}
	if err := im.AddCustomAnalyzer(ngramAnalyzerName, map[string]any{
		"type":          custom.Name,
		"tokenizer":     unicode.Name,
		"token_filters": []any{lowercase.Name, "meerkat_edge_ngram"},
	}); err != nil {
		return fmt.Errorf("register n-gram analyzer: %w", err)
	}
	return nil
}
