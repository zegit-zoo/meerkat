# How `mk search` works

Meerkat's search is a keyword index over the configured markdown wiki —
embedded at build time by default, or a directory resolved at runtime
(`--kb-dir`/`MEERKAT_KB_DIR`, or a `content-source.yaml`; see the README).
It runs entirely in-process — no external service, no network, no
embedding model.

## TL;DR

| | |
|---|---|
| Backend | [Bleve](https://blevesearch.com/) — a pure-Go BM25 search engine |
| Index | In-memory only; rebuilt at startup |
| Cold start | ~150 ms on 730+ pages |
| Per-query latency | ~5 ms warm |
| Boosts | title × 5, id × 3, body × 1 |
| Highlights | yes — `Snippet` field with `<mark>` markers |

## Why BM25 (and not embeddings)

- The KB is bounded to a few thousand pages. BM25 is plenty for
  page-name lookups (e.g. "Rate-Limiting", "Idempotency") and
  keyword queries.
- An embedding model would mean shipping ONNX/PyTorch (10×–100×
  binary size), or going over the network at every query. We deliberately
  trade some recall for "single binary, no runtime deps".
- The day this stops being enough is the day someone files an issue
  like "I searched for `cryptography key handling` and the relevant
  page wasn't in the top 10". Then we add a vector index alongside
  Bleve, behind the same `Index.Query` interface.

## Index construction

```go
// internal/search/index.go
func New() (*Index, error) {
    pages, _ := kb.List()      // walk the configured FS — embedded, or a runtime-resolved directory
    bidx, _ := bleve.NewMemOnly(buildMapping()) // in-RAM Bleve
    batch := bidx.NewBatch()
    for _, p := range pages {
        batch.Index(p.ID, map[string]any{
            "id":    p.ID,
            "title": p.Title,
            "body":  p.Body,
        })
    }
    bidx.Batch(batch)
    return &Index{bleve: bidx, pages: pageMap}, nil
}
```

Three indexed fields with the standard analyser (lowercase, stop-words,
simple stemming). Bleve's mapping wires this up:

```go
func buildMapping() *mapping.IndexMappingImpl {
    im := bleve.NewIndexMapping()
    docMap := bleve.NewDocumentMapping()
    title := bleve.NewTextFieldMapping(); title.Analyzer = "standard"
    id    := bleve.NewTextFieldMapping(); id.Analyzer    = "standard"
    body  := bleve.NewTextFieldMapping(); body.Analyzer  = "standard"
    body.Store = true                       // needed for Highlight
    docMap.AddFieldMappingsAt("title", title)
    docMap.AddFieldMappingsAt("id",    id)
    docMap.AddFieldMappingsAt("body",  body)
    im.AddDocumentMapping("_default", docMap)
    return im
}
```

## Querying

```go
// Boosted disjunction over title + id + body.
titleQ := bleve.NewMatchQuery(q); titleQ.SetField("title"); titleQ.SetBoost(5.0)
idQ    := bleve.NewMatchQuery(q); idQ.SetField("id");       idQ.SetBoost(3.0)
bodyQ  := bleve.NewQueryStringQuery(q)              // baseline 1.0

combined := bleve.NewDisjunctionQuery(titleQ, idQ, bodyQ)
req := bleve.NewSearchRequestOptions(combined, limit, 0, false)
req.Highlight = bleve.NewHighlight()
req.Highlight.AddField("body")
```

The boost ratio (5 / 3 / 1) was tuned empirically against mk's
~200-page corpus and carried over here. Page-name queries now
reliably surface the actual page (not an incidental mention) as
the top hit; see `internal/search/index_test.go::TestQuery_RankByTitle`.

## Search syntax

The body field uses Bleve's QueryStringQuery, which gives users
some power without needing docs:

| Pattern | Meaning |
|---------|---------|
| `circuit breaker` | both terms anywhere (AND) |
| `"circuit breaker"` | exact phrase |
| `+retry -cache` | must contain retry, must not contain cache |
| `title:Foo` | match against the title field only |
| `body:foo*` | wildcard suffix in body |
| `cache OR queue` | either term |

Field targeting against `id` and `title` works alongside the boost,
so `title:retry` returns only pages whose title contains "retry".

## The staged planner: exact, then fuzzy, then prefix

A query runs through up to three stages (`internal/search/planner.go`),
each only when the previous one found **nothing**:

| Stage | What runs | Reported as |
|---|---|---|
| `exact` | the BM25 query above — title ×5, id ×3, body, category boosts | `meerkat.search.stage="exact"` |
| `fuzzy` | per term: one edit allowed from 5 characters, two from 8; shorter terms stay exact | `"fuzzy"` |
| `prefix` | per term of 3+ characters: prefix match on title/id/body | `"prefix"` |

So `datadgo monitor serach` finds the Datadog page at the fuzzy stage,
`pager` finds PagerDuty at the prefix stage, and a query that hits
exactly costs exactly what it always cost. The stage is on every
result (`Result.Stage`), on the search span, and on
`meerkat_search_total{outcome,stage}` — the fallback rate
(`fuzzy + prefix` over the total) is the signal that a vendor name or a
Swedish/English compound is missing from the content and should be
added at the source.

Queries that use bleve syntax — field targeting, quoted phrases,
wildcards, `~`, `+`/`-` operators — are **never rewritten**: the author
asked for precision. An in-word hyphen (`drift-detektering`) is a
joiner, not an operator.

Cost, 500 synthetic pages of ~120 words, `go test -bench
QueryStaged` on an 8-core laptop:

| Case | Time per query |
|---|---|
| exact hit | 0.57 ms |
| exact miss → fuzzy hit | 0.41 ms |
| exact and fuzzy miss → prefix | 0.36 ms |

A fallback pass costs about as much as the exact pass it follows,
well inside the 5 ms p95 budget; a miss is cheaper than a hit because
there are no snippets to build.

## Frontmatter as fields

`type`, `status`, `subcategory` and `tags` are indexed as **keyword
fields** beside `category` and `owner` (exact values, not tokens,
excluded from `_all` so they never match free text). `Index.Match`
answers `mk_list`-style filters — type, status, category,
subcategory, tags (all must be present), owner, ID prefix — as query
clauses instead of a post-list walk: ~16 µs on the 500-page corpus
above (`BenchmarkMatch`). The `mk_list`/`POST /list`/`mk list`
surfaces still filter through `kb.Filter` today; switching them to
`Match` is a small follow-up once the index is guaranteed built before
`list` is served.

## Title analyzer for a hub tier (`layout.analyzer: ngram`)

A hub tier is a small collection of routing pages whose titles are
vendor and system names. With `layout.analyzer: ngram` in
`content-source.yaml`, titles are indexed as edge n-grams of 3–8
characters, so `datadg` — or the first letters of a name someone
half-remembers — matches "Datadog" at the exact stage. It multiplies
the title term count, so it is refused for a corpus over 1 MiB
(`search.MaxNgramCorpusBytes`, the hub tier size the mob design sets).
Bodies keep the standard analyzer.

## Cold-start budget

Built once per binary launch, kept in memory for the life of the
process:

| Surface | When index is built |
|---------|---------------------|
| `mk search …` | per-invocation (CLI is short-lived; cold-start is the cost) |
| `mk mcp serve` | once at startup, shared across tool calls |
| `mk http serve` | once at startup, shared across requests |

For long-running surfaces the cost is paid once. For one-shot CLI
calls the ~150 ms is acceptable for an interactive search; the
alternative (persistent index file on disk) would trade complexity
for a small saving on a tool you usually invoke a handful of times
in a session.

## Performance regression target

```bash
go test -bench=. ./internal/search/...
```

Current numbers (M-series Mac):

```text
BenchmarkNew    ~150ms/op   (cold-start of in-memory index)
BenchmarkQuery  ~5ms/op     (warm query against 730+ pages)
```

If either gets significantly worse on a Bleve bump, the regression
shows up immediately. The benchmarks live alongside the unit tests
so they ride for free in CI.
