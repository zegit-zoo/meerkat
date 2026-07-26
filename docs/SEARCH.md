# How `mk search` works

Meerkat's search is a keyword index over the embedded markdown wiki.
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
    pages, _ := kb.List()                       // walk embedded FS
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

The boost ratio (5 / 3 / 1) was tuned empirically against btkb's
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

## Frontmatter as fields (planned)

Frontmatter (`category`, `owner`, `status`, `tags`, …) is parsed into
`kb.Page.Front` but **not yet indexed by search**. The CLI/MCP/HTTP
filters compose post-search via `kb.Filter` helpers. The day we
need `mk search "owner:team-payments tier-1"` to work in the search
syntax itself is when we extend `buildMapping()` with a low-boost
keyword field per Frontmatter key.

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

```
BenchmarkNew    ~150ms/op   (cold-start of in-memory index)
BenchmarkQuery  ~5ms/op     (warm query against 730+ pages)
```

If either gets significantly worse on a Bleve bump, the regression
shows up immediately. The benchmarks live alongside the unit tests
so they ride for free in CI.
