# How `mk search` works

Meerkat's search is a keyword index over whatever markdown wiki the binary
resolved at startup — by default something loaded at runtime: a directory on
disk, a verified HTTPS archive, a GCS or S3 object, or several of those
mounted as named collections (`--kb-dir`/`MEERKAT_KB_DIR`, or a
`content-source.yaml`; see the README's
[Loading content](../README.md#loading-content)). Content embedded in the
binary is indexed the same way, but only a build from source has any. The
index itself is built and queried entirely in-process — no external service,
no embedding model, and no network once the content is resolved.

## TL;DR

| | |
|---|---|
| Backend | [Bleve](https://blevesearch.com/) — a pure-Go full-text search library; the in-memory index scores with **TF-IDF** |
| Index | In-memory only; rebuilt at startup |
| Cold start | ~150 ms on 730+ pages |
| Per-query latency | ~5 ms warm |
| Boosts | title × 5, id × 3, description × 2, hint × 1, body × 1; then a per-`type` multiplier, configurable per collection |
| Highlights | yes — `Snippet` field with `<mark>` markers; body first, then frontmatter `description`, then a pointer's `hint` |

## Why keyword search (and not embeddings)

The in-memory index is bleve's `upsidedown` type (`bleve.NewMemOnly`),
which scores with **TF-IDF** — earlier versions of this page said BM25.
bleve implements BM25 only for its `scorch` index type, so setting
`ScoringModel: bm25` on this index changes nothing (measured: identical
results on the mk-mpe retrieval eval). Switching index types is tracked
in [#101](https://github.com/zegit-zoo/meerkat/issues/101). The
practical difference: TF-IDF has no term-frequency saturation and its
field-length norm strongly favours short fields, which is why the boost
table below is nominal rather than exact.

- The KB is bounded to a few thousand pages. Keyword scoring is plenty for
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
            "id":          p.ID,
            "title":       p.Title,
            "description": p.Front.Description, // OKF's one-line summary
            "hint":        p.Front.Hint,        // a pointer's reason to follow it
            "body":        p.Body,
        })
    }
    bidx.Batch(batch)
    return &Index{bleve: bidx, pages: pageMap}, nil
}
```

Five indexed text fields with the standard analyser (lowercase,
stop-words, simple stemming). Bleve's mapping wires this up:

```go
func buildMapping() *mapping.IndexMappingImpl {
    im := bleve.NewIndexMapping()
    docMap := bleve.NewDocumentMapping()
    title := bleve.NewTextFieldMapping(); title.Analyzer = "standard"
    id    := bleve.NewTextFieldMapping(); id.Analyzer    = "standard"
    body  := bleve.NewTextFieldMapping(); body.Analyzer  = "standard"
    desc  := bleve.NewTextFieldMapping(); desc.Analyzer  = "standard"
    hint  := bleve.NewTextFieldMapping(); hint.Analyzer  = "standard"
    body.Store = true                       // needed for Highlight
    desc.Store = true                       // same — it can supply the snippet
    hint.Store = true                       // same
    docMap.AddFieldMappingsAt("title",       title)
    docMap.AddFieldMappingsAt("id",          id)
    docMap.AddFieldMappingsAt("description", desc)
    docMap.AddFieldMappingsAt("hint",        hint)
    docMap.AddFieldMappingsAt("body",        body)
    im.AddDocumentMapping("_default", docMap)
    return im
}
```

## Querying

```go
// Boosted disjunction over title + id + description + hint + body.
titleQ := bleve.NewMatchQuery(q); titleQ.SetField("title");       titleQ.SetBoost(5.0)
idQ    := bleve.NewMatchQuery(q); idQ.SetField("id");             idQ.SetBoost(3.0)
descQ  := bleve.NewMatchQuery(q); descQ.SetField("description");  descQ.SetBoost(2.0)
hintQ  := bleve.NewMatchQuery(q); hintQ.SetField("hint");         hintQ.SetBoost(1.0)
bodyQ  := bleve.NewQueryStringQuery(q)              // baseline 1.0

combined := bleve.NewDisjunctionQuery(titleQ, idQ, descQ, hintQ, bodyQ)
req := bleve.NewSearchRequestOptions(combined, limit, 0, false)
req.Highlight = bleve.NewHighlight()
req.Highlight.AddField("body")
req.Highlight.AddField("description")
req.Highlight.AddField("hint")
```

The title/id/body ratio (5 / 3 / 1) was tuned empirically against mk's
~200-page corpus and carried over here; `description` was slotted into
it afterwards, at 2. Page-name queries reliably surface the actual page
(not an incidental mention) as the top hit; see
`internal/search/index_test.go::TestQuery_RankByTitle`.

| Field | Boost | Why |
|---|---|---|
| `title` | × 5 | the page's name: a title match is almost always the page asked for |
| `id` | × 3 | as name-like as a title, and what a cross-reference cites |
| `description` | × 2 | OKF's hand-written one-line summary: deliberate wording, denser than prose, but not a name |
| `hint` | × 1 | a pointer's one-sentence reason to follow it — about the page at the other end |
| `body` | × 1 | baseline — the prose itself |

`description` was added in [#83](https://github.com/zegit-zoo/meerkat/issues/83).
It sits **below** `title` and `id`, so title-first page-name lookup is
exactly what it was; and **above** `body`, because a term someone put in
a one-line summary is a stronger signal than the same term mentioned in
passing halfway down a page. Two mirror-image fixtures pin both halves
of that claim in
`internal/search/description_test.go::TestDescription_BoostSitsBetweenTitleAndBody`:
the same pair of strings swapped between the fields, so the only thing
separating the two pages is which field the query matched in.

The multipliers are **nominal**. Under TF-IDF a clause's score also
carries the field-length norm, which is large for a one-line field and
small for `_all`, the whole page's text. The query-string ("body") clause
runs against `_all`. So a short field outweighs the same term in a long
body by far more than its multiplier says. Two things keep that in
check:

- `description` and `hint` are **not** in `_all` (#85 review). They are
  scored once, through their own clause, rather than a second time
  through `_all` with twice the coordination factor. On the mk-mpe eval
  this took dev hit@1 from 0.53 to 0.57, with holdout unchanged.
- The trade-off is recorded, not accidental. A page that only
  **mentions** a term in its one-line description still outranks one
  whose 300-word body uses it five times. With the double count it won
  by about 40×; it now wins by about 7×. The dev split did not favour a
  lower weight: ×1 was within noise, and ×0.5 lost MRR.
  `TestDescription_OneLineMentionVsDenseBody` pins the ordering and
  bounds the margin.

`hint` joined in [#88](https://github.com/zegit-zoo/meerkat/issues/88)
with a clause of its own at the body's weight. On a pointer it is
often the only sentence that names what is at the other end in the
words a searcher types, so a term there must find the pointer. But it
describes the page at the other end, and on a leaf collection a ×2 hint
let citation pointers take questions a concept page answers (the mk-mpe
eval's dev split: −0.05 MRR at ×2, −0.03 at ×1, both against no hint).
`internal/search/hint_test.go::TestHint_WeightsAgainstTitleAndBody`
pins title over hint and a hint-only match over a body-only one, with
type boosting switched off.

## Type boosts, per collection (`search.type_boosts`)

After the field-boosted relevance score, a hit is multiplied by a weight for
its frontmatter `type`. The defaults (`search.DefaultTypeBoosts`) are
pointer × 4, skill × 2, example × 1.5, and they assume a **hub**: a
small set of routing pages above thin content, where "the pointer
outranks the page" is the point. The design and the multiplier's rule
are in [docs/design/links.md](design/links.md#ranking).

A **leaf** collection is the opposite shape. When it cites its sources
through pointers — one per book chapter or article — the × 4 lets a
citation outrank the page that answers the question. A collection
overrides the defaults for itself in `content-source.yaml`:

```yaml
collections:
  - name: mk-mpe
    type: local
    path: .
    search:
      type_boosts: {pointer: 1.0}   # leaf: pointers are citations, not routes
```

| `type_boosts` | Meaning |
|---|---|
| absent, or `null` | the defaults |
| `{}` | no type boosting at all |
| `{pointer: 1.0}` | exactly this map; a type it does not list is unboosted |

Weights must be positive and finite, and the file is refused at load
otherwise. A `0` is refused rather than read as "hide this type": the
ranker treats a non-positive weight as no boost, so it would silently
mean × 1. Keys are not checked against known types, because a boost for
a type no page carries yet is inert rather than wrong. Each collection
builds its own index, so one collection's map never reaches another's
ranking (`internal/collections/typeboosts_test.go`).

The case that motivated the knob is a leaf of 196 pointers against 61
concept pages, where a labelled retrieval eval put a chapter pointer
first for most questions a concept page answers; the measurements are
on [#88](https://github.com/zegit-zoo/meerkat/issues/88).

## Search syntax

The body field uses Bleve's QueryStringQuery, which gives users
some power without needing docs:

| Pattern | Meaning |
|---------|---------|
| `circuit breaker` | both terms anywhere (AND) |
| `"circuit breaker"` | exact phrase |
| `+retry -cache` | must contain retry, must not contain cache |
| `title:Foo` | match against the title field only |
| `description:Foo` | match against the frontmatter one-liner only |
| `hint:Foo` | match against a pointer's hint only |
| `body:foo*` | wildcard suffix in body |
| `cache OR queue` | either term |

Field targeting against `id`, `title`, `description` and `hint` works
alongside the boost, so `title:retry` returns only pages whose title
contains "retry", and `description:retry` only those whose frontmatter
summary does.

## The staged planner: exact, then fuzzy, then prefix

A query runs through up to three stages (`internal/search/planner.go`),
each only when the previous one found **nothing**:

| Stage | What runs | Reported as |
|---|---|---|
| `exact` | the query above — title ×5, id ×3, description ×2, hint ×1, body, category boosts | `meerkat.search.stage="exact"` |
| `fuzzy` | per term: one edit allowed from 5 characters, two from 8; shorter terms stay exact | `"fuzzy"` |
| `prefix` | per term of 3+ characters: prefix match on title/id/description/hint/body | `"prefix"` |

All three stages search the same five fields with the same relative
boosts (`termClauses` in `planner.go`), so a typo or a half-typed word
reaches a frontmatter description exactly as it reaches a title or a
body — `capybra` and `capy` both find a page whose only "capybara" is in
its `description`
(`internal/search/description_test.go::TestDescription_FoundAtEveryStage`).

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

### Type boosts are applied before the cut

Every stage ends in the same step (`run` in `index.go`): score, apply
the per-`type` multiplier (`search.DefaultTypeBoosts`, pointer ×4 —
see [docs/design/links.md](design/links.md#ranking)), and only then cut
to the limit. Before [#87](https://github.com/zegit-zoo/meerkat/issues/87)
the cut came first: bleve returned the raw top-`limit`, so a pointer
whose raw score sat just outside it was dropped even when its boosted
score would have won, and `--limit 1` and `--limit 10` could disagree
about the top hit. Results are now **prefix-stable**: the first `n`
results of any larger limit are the results of limit `n`, at every
stage and for every viewer
(`internal/search/boostcut_test.go::TestTypeBoosts_ResultsArePrefixStable`).

The multiplier runs inside the query: `run` wraps the stage query in a
bleve custom score (`query.NewCustomScoreQueryWithScorer`) whose
callback multiplies each candidate's score by its page's weight, so
bleve's own collector ranks and cuts on the final score, and breaks
ties the same way whatever the limit. Highlighting still covers only the
hits returned. A collection whose weights are all 1 — or that has none,
`type_boosts: {}` — runs the query unwrapped.

| Case | Time per query |
|---|---|
| exact hit, no typed pages (as above) | 0.60 ms |
| exact hit, one page in ten a pointer (`BenchmarkQueryStagedExactBoosted`) | 0.61 ms, from 0.59 ms before #87 |

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

`description` and a pointer's `hint` are the frontmatter fields that
are **not** keyword facets. `description` is OKF's one-line summary
(SPEC.md §4.1), prose rather than a value to filter on, so it gets the
body's standard analyser — including
on a hub tier, where titles are n-grammed but a summary is still a
sentence. It has its own clause in every stage and is kept out of
`_all` (see the boost section above), and `description:foo` targets it
directly. It is stored as well as indexed, so a page matched only on
its summary still shows a snippet: `run` prefers the body fragment and
falls back to the description's, chosen from the fields bleve reports
term locations in rather than from the fragments themselves (bleve
returns a fragment for every highlighted field, matched or not, so the
head of a long body would otherwise mask the real match).
Incrementally indexed pages carry the field too — `Index.Put` and the
bulk build share one `indexDoc` — so a memory saved mid-session is
searchable by its description immediately
(`internal/search/put_test.go::TestPut_IndexesTheFrontmatterDescription`).
`hint` is mapped the same way for the same reasons (at the body's
weight — see the boost table), and supplies the snippet last, after the
body and the description.

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
