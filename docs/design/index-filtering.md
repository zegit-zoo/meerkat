# Assessment: index-time frontmatter filtering for very large knowledge bases

**Status:** Assessment only — no implementation decision made, no production
code added · **Builds on:** [multi-collection.md](multi-collection.md) ·
**Issue:** #1

*(This is not a spec for a shipped feature. It answers the questions issue
#1 asked, with measurements, and recommends a plan — implementing it is
future work.)*

## The ask

Let an operator launch meerkat so only pages whose frontmatter matches a
filter get indexed, so a knowledge base far bigger than any single
deployment needs doesn't cost that deployment startup time or memory it
will never use. The building blocks already exist:

- `kb.FilterFunc` and its presets `ByCategory`/`ByStatus`/`ByOwner`/
  `ByPrefix`/`ByType` (`internal/kb/content.go`).
- `mk list --prefix/--category/--status/--owner/--type`, which already
  compose these presets — but **after** `kb.List()` has read and parsed
  every page (`internal/cli/list.go`).
- `search.NewFromPages(pages, ...)` (`internal/search/index.go`), the
  injection point that builds the bleve index from an explicit page
  slice rather than always calling `kb.List()` itself.
- Since #8, `internal/collections.Collection.Index()` is exactly where a
  per-collection index gets built lazily, once, from `c.Pages()`
  (`internal/collections/registry.go`) — the natural seam for a
  per-collection filter to cut in.

Today, filtering is **query-time**, over a **fully built** index (or, for
`mk list`, over a fully-read page slice). The ask is **index-time**:
narrow what gets indexed (and read) in the first place.

## Q1 — What happens to an excluded page?

**`mk show`/`mk_show` is unaffected by any index filter, by design, and
this must be stated plainly: an index filter is not an access-control
mechanism.**

`kb.Load`/`kb.LoadFS` (what `mk show` and `collections.Collection.Load`
call, in `internal/collections/registry.go`) open exactly one file by
its ID. They never consult the search index and never walk the corpus, so
they have no way to know a filter exists unless one is added at that call
site specifically — which this assessment recommends against. A page
excluded from the index remains reachable by its exact ID. This is
already true of every existing `kb.FilterFunc` preset (`--category`,
`--status`, ...): a page filtered out of `mk list --status=reviewed` has
always still been one `mk show <id>` away. An index filter is the same
kind of filter — an operator narrowing what a *deployment* is organised
around, not who may see what — so it should behave the same way.

**Say it as plainly as the issue asked for:** if you need "some pages are
not visible to some callers," you need the per-collection **authorization**
that [multi-collection.md's follow-ups](multi-collection.md#follow-ups)
describe, not this filter. That follow-up is explicit that an
unauthorized collection "must be invisible (as if not mounted), not
merely denied, or the ambiguity error becomes an existence oracle" —
i.e. it is a real security boundary and has to close the ID-guessing hole
this filter deliberately leaves open. An index filter and per-collection
authorization solve different problems and must not be conflated: this
filter bounds *cost* for an operator who already trusts every caller with
every page; authorization bounds *visibility* for callers who must not be
trusted with all of it. A deployment that needs both applies both — the
filter to keep the index small and relevant, authorization to keep pages
away from callers who shouldn't see them at all (including by direct ID).

**`mk list` should respect the same filter as search, per collection.**
Two options exist:

1. Filter only `Index()`, leave `Pages()` (and hence `mk list`) showing
   the whole corpus.
2. Filter both `Pages()` and `Index()` for that collection; `Load` alone
   stays unfiltered.

Option 2 is recommended. `mk list`'s purpose is "what can I find" — if a
filtered collection's `list` still enumerates every page in a
far-bigger-than-needed corpus, it inherits exactly the O(N) read/parse
cost this whole feature exists to avoid (see Q2: that cost is dominated by
opening and parsing every file, which `list` cannot skip either, once it
enumerates everything to show it). It would also be actively confusing:
`mk list` showing ten thousand rows that `mk search` can never surface
gives no indication anything is filtered at all. Making `list` and
`search` agree on one collection's visible set, with `show` as the one
documented, deliberate exception for "I already know the ID," is a single
coherent story an operator (or an auditor) can hold in their head, and
costs nothing extra to implement — see the plan in Q2/§Plan; whichever of
approaches (b)/(c)/(d) backs `Index()` also backs `Pages()`, since it
already computes exactly the filtered page slice `Pages()` needs to
return.

**Interaction with the multi-collection Registry:** none of `Registry.Get`,
`Registry.Search`, `Registry.Show` or its ambiguity rule (all in
`internal/collections/registry.go`) need to change. `Registry.Show`
calls `c.Load(pageID)` per targeted collection, which — per the above —
stays filter-blind; a filtered-out page is still a normal, unambiguous
`Load` result. `Registry.Pages`/`Registry.Search` call `c.Pages()`/
`c.Index()`, which is exactly where the filter already lives if it's
implemented per Option 2. No new ambiguity case is introduced: a page
excluded from collection A's index is invisible to a *bare, unqualified*
`mk show <id>` search across every collection only in the sense that
collection A won't contribute a match for it via `Pages()`/`Index()` — but
`Show` doesn't consult either of those, it calls `Load` directly, so this
doesn't change. Worth documenting explicitly (this doc is that
documentation) so nobody mistakes "excluded from A's index" for "excluded
from A."

## Q2 — Does index-time filtering bound the cost that matters? (measured)

### Method

Benchmarks and a synthetic-corpus generator live in
[`internal/indexfilter`](../../internal/indexfilter) (`corpus_test.go`,
`bench_test.go`) — entirely inside `_test.go` files, so `go build ./...`
never links them into a binary and plain `go test ./...` / `make test`
never runs their bodies (Go only executes `Benchmark*` functions when
`-bench` is passed). Run them with:

```
go test -run '^$' -bench . -benchmem -benchtime=1x ./internal/indexfilter/...
```

`GenerateCorpus(root, n, seed)` deterministically writes `n` synthetic
pages under `root/wiki/<category>/page-NNNNN.md` (the content-repo layout
`internal/kbdir.FS` adapts onto `kb`'s `content/...` paths), with
frontmatter carrying `category`/`subcategory`/`owner`/`status`/`language`/
`tags`/`extra` and a body of 2–4 KB of realistic-shaped (not
single-repeated-word) English text — "a few KB," per the issue. Ten
categories are assigned round-robin, so exactly 1/10 (~10%) of pages carry
`category: ops`, the value every filtered benchmark below selects on,
matching "a filter matches ~10%."

Four approaches are measured, at 1,000 / 10,000 / 50,000 pages:

- **(a) full build** — today's only option: `kb.ListFS` (open + read +
  parse every file fully) then `search.NewFromPages` over all of it.
- **(b) filtered post-list** — the simplest possible "index-time filter":
  `kb.ListFS` (still reads and parses every file — same cost as (a)'s
  first half) then `kb.Filter(pages, kb.ByCategory(...))` before indexing.
  This is what `mk list`'s existing flags already do, just relocated from
  a CLI flag to something applied automatically at startup.
- **(c) frontmatter-only filtered** — every file is opened once, but the
  body is read only for the ~10% that match; a non-match costs one file
  open plus a few hundred bytes of YAML, never a multi-KB body read.
- **(d) manifest filtered** — a small pre-built sidecar (id → category/
  status/owner/tags/language, no body) is decoded once; only the
  matching ~10% of files are ever opened at all. The one-time cost of
  *building* that sidecar is reported separately
  (`BenchmarkManifestBuild`), since a real design would pay it at ingest
  time or on a periodic refresh, never on the request/startup path.

`BenchmarkListOnly` isolates `kb.ListFS`'s own cost (no bleve at all),
to separate "reading the corpus" from "indexing it" — see the surprise
below.

**Machine:** Apple M2 Pro, 10 cores, macOS/arm64, Go 1.27.0 (module
pins Go 1.26). One laptop, not a benchmarking rig — expect run-to-run
variance from thermal/power throttling under sustained load (observed:
±30–40% on repeated runs of the same 50k-page case) and treat every
number here as order-of-magnitude, not a precise regression target.
Also note: this checkout's embedded KB is empty (the public repo ships no
`content-source.yaml`, per
[content-sources.md](content-sources.md#background--state-before-this-spec)),
so `docs/SEARCH.md`'s documented "~150ms on 730+ pages" baseline
**could not be reproduced from this repo as checked out** — it describes
a deployment with real content this tree doesn't have. Every number below
is instead from the synthetic corpus described above.

### Results

Wall time, `go test -bench . -benchmem -benchtime=1x` (one op per case —
enough to see order-of-magnitude differences, not enough to average out
the machine noise above):

| approach | 1,000 pages | 10,000 pages | 50,000 pages |
|---|---:|---:|---:|
| list only (no index) | 88 ms | 829 ms | 4.32 s |
| (a) full build | 478 ms | 7.05 s | 98.1 s (isolated re-run: 140.6 s) |
| (b) filtered post-list (~10%) | 211 ms | 1.92 s | 9.99 s (isolated: 7.12 s) |
| (c) frontmatter-only filtered | 103 ms | 1.07 s | 5.77 s (isolated: 5.88 s) |
| (d) manifest filtered (steady-state) | 45 ms | 500 ms | 3.15 s (isolated: 3.36 s) |
| (d) manifest **build** (one-time/amortized) | 70 ms | 663 ms | 3.24 s |

Peak resident memory at 50,000 pages (`/usr/bin/time -l`, "maximum
resident set size," isolated single-process runs — `-benchmem`'s B/op is
cumulative garbage allocated, not peak RSS, so this is a second,
independent measurement):

| approach | peak RSS @ 50k pages |
|---|---:|
| list only | 0.62 GB |
| (a) full build | 9.48 GB |
| (b) filtered post-list | 1.41 GB |
| (c) frontmatter-only filtered | 1.35 GB |
| (d) manifest filtered | 1.32 GB |

Allocations (`-benchmem` allocs/op, same run as the wall-time table):

| approach | 1,000 | 10,000 | 50,000 |
|---|---:|---:|---:|
| (a) full build | 11.75M | 135.4M | 750.3M |
| (b) filtered post-list | 1.35M | 15.2M | 82.4M |
| (c) frontmatter-only filtered | 1.14M | 14.0M | 73.1M |
| (d) manifest filtered | 0.96M | 11.9M | 65.4M |

### What this shows

1. **Bleve index construction, not file I/O or YAML parsing, dominates
   cost at scale.** `BenchmarkListOnly` (read + parse all 50,000 files,
   build no index) took 4.3s / 0.62 GB; the *full build* of the same
   corpus took 98–141s / 9.48 GB. Indexing the corpus costs roughly
   **20–30× more than reading it**. This inverts the assumption the issue
   posed the question under (that avoiding file reads is the main lever)
   — the main lever is avoiding feeding non-matching *documents* to
   bleve, not avoiding opening non-matching *files*.
2. **Because of (1), the simplest possible approach — (b), filter after
   a full `kb.ListFS`, same as `mk list`'s existing flags — already
   captures the overwhelming majority of the benefit.** At 50k pages it
   is ~10–14× faster and uses ~7× less peak memory than the unfiltered
   build, using code that already exists (`kb.Filter` + any `kb.ByXxx`
   preset) with no new parsing logic at all.
3. **The fancier options (c)/(d) give a real but secondary further gain**
   — roughly another 2–3× on top of (b) at 50k pages in wall time, and a
   modest allocation-count reduction (10–20%) — by also cutting the O(N)
   file-open/parse pass that (b) still pays. They do **not** meaningfully
   reduce *peak memory* further: (b), (c) and (d) land within a few
   percent of each other (1.3–1.4 GB), because peak RSS in all three is
   dominated by the same thing — bleve building an index over the ~10%
   matched subset — not by how many files were opened to find that
   subset.
4. **Full, unfiltered indexing scales worse than linearly in this
   range**: 10× more pages (1k→10k) cost ~15× the time; a further 5×
   (10k→50k) cost ~14–20× the time (98–141s vs 7.05s). Whether that's
   genuine superlinear indexing cost or GC pressure from sustained
   multi-GB allocation (both plausible, and not disentangled by this
   assessment), the practical conclusion is the same: an unfiltered
   index over a corpus "far bigger than any deployment needs" doesn't
   just cost proportionally more, it costs *disproportionately* more —
   strengthening, not weakening, the case for doing this at all.
5. **The GCS backend (#8) changes the calculus for (c)/(d).** All the
   above is against local disk, where "open a file" is cheap. For a
   `type: gcs` collection in `prefix:` mode — every object under a prefix
   mirrored as a tree (`docs/design/multi-collection.md`'s "GCS backend"
   section) — "never open a non-matching file" also means "never issue a
   GCS GET for it," which is a real network/API-cost/latency dimension
   this local-disk benchmark cannot see at all. That tips the
   cost-benefit of (c)/(d) earlier for object-storage-backed collections
   than the page-count thresholds below suggest for local/`type: local`
   ones.

## Q3 — Filter expression syntax

**Recommendation: a per-collection config key, not new CLI flags.**
`mk list --prefix/--category/--status/--owner/--type` are *per-invocation,
query-time* choices — "show me this slice, right now." An index filter is
a *deployment-time* choice — "this collection only ever contains this
slice" — the same kind of decision as `path:`/`bucket:`/`layout:`, which
already live in `content-source.yaml`, not on the command line. The issue
frames it exactly this way ("an index filter most naturally becomes a
per-collection key in the `collections:` list"), and this assessment
agrees: no new CLI flags, one new YAML key.

Since `content:` and each `collections:` entry already share one `Source`
struct (`Collection` is `Source` inlined plus a `name`, in
`internal/contentsource/config.go`), adding the field to `Source`
gives it to both shapes for free, exactly like `Layout` already is:

```yaml
# Single-collection form — content: is a Source like any other.
content:
  type: local
  path: ../meerkat-kb
  index_filter:
    category: [policies, runbooks]   # OR within a field
    status: reviewed                 # scalar shorthand for a one-value list
    owner: team-platform

# Multi-collection form — each entry is the same Source, plus a name.
collections:
  - name: runbooks
    type: local
    path: ../runbooks-kb
    index_filter:
      status: [reviewed, approved]
      tags: [tier-1, customer-facing]     # ANY of these tags — see Q5
      extra.region: us-east                # dotted reachability into Extra

  - name: architecture
    type: gcs
    bucket: my-org-knowledge
    prefix: architecture/live/
    index_filter:
      category: adr
    # No filter on team-notes below: every collection can independently
    # choose to filter or not.

  - name: team-notes
    type: gcs
    bucket: my-org-knowledge
    prefix: notes/live/
```

Sketch of the shape (illustrative — this assessment does not implement
it):

```go
// IndexFilter narrows which pages a collection indexes and lists (see
// docs/design/index-filtering.md). Every configured field must match
// (AND across fields); each field accepts one value or a list, and a
// page matches a list field if it matches ANY entry in it (OR within a
// field). A zero-value IndexFilter (the default, all fields empty)
// keeps everything, matching kb.FilterFunc(nil)'s existing convention.
type IndexFilter struct {
    Category    stringOrList `yaml:"category,omitempty"`
    Subcategory stringOrList `yaml:"subcategory,omitempty"`
    Owner       stringOrList `yaml:"owner,omitempty"`
    Status      stringOrList `yaml:"status,omitempty"`
    Language    stringOrList `yaml:"language,omitempty"`
    Type        stringOrList `yaml:"type,omitempty"`    // OKF's concept-kind field
    Prefix      string       `yaml:"prefix,omitempty"`  // page-ID prefix, like --prefix
    Tags        []string     `yaml:"tags,omitempty"`    // ANY-of, see Q5
    // Extra reaches into Frontmatter.Extra by a single top-level key —
    // "extra.region: us-east" in YAML unmarshals to Extra["region"].
    // One level deep to start (Extra values are `any`, so deeper dotted
    // paths are a plausible future extension, not implemented here).
    Extra map[string]stringOrList `yaml:"-"` // populated from "extra.<key>" keys during Source decoding
}
```

`stringOrList` is the same "accept a bare scalar or a list" tolerance
`kb.VerifiedList.UnmarshalYAML` already establishes for `verified:`
(`internal/kb/content.go`) — reuse that idiom rather than
inventing a second one. The `extra.<key>` keys need a small amount of
custom decoding (they aren't a fixed Go field name), but that's a solved
problem in this codebase too: `splitFrontmatter`'s second YAML pass (same
file) already collects "every key not in a known set" into a map for
exactly this reason.

## Q4 — Provenance: should `mk version`'s `kb_source` reflect an active filter?

**Recommendation: yes, but as a separate field alongside each collection's
existing provenance, not folded into `kb_source` itself** — the same
pattern #8 used for `Type` (`collectionInfo{Name, Type, Source}` in
`internal/cli/version.go`; `Type` rides next to `Source`, it isn't
concatenated into the provenance string). Precedent: `Registry.Provenance`
(`internal/collections/registry.go`) deliberately keeps `kb_source` a
single string for the common single-collection case and pushes all
per-collection detail into the `collections` array rather than growing
one field's shape based on config — the same reasoning applies here.

Concretely: add `Filter string` to `collectionInfo` (and to `mk list
--collections`'s `entry` struct in `internal/cli/list.go`, which
already reports per-collection `Type`/`Source`/`Pages` the same way),
populated as a short human-readable summary (e.g.
`"category=policies,runbooks status=reviewed"`) when
`Collection.Source.IndexFilter` is non-zero, empty otherwise. Two reasons
this matters enough to be explicit, not just discoverable by reading a
config file nobody attached to the running process:

- **It's the honesty mechanism for Q1's "not a security boundary."**
  Since an index filter narrows what's *searchable* without narrowing
  what's *loadable by ID*, anyone auditing a deployment needs a way to
  see, from the running process, that collection X currently excludes
  some of its own corpus from search — otherwise "why didn't `mk search`
  find a page `mk show` can still open" has no answer short of reading
  the operator's config file (which a remote caller of `mk_version` over
  MCP/HTTP can't do at all).
- It follows the same "what this binary actually serves" spirit `mk
  version`'s per-collection array already exists for (see
  `multi-collection.md`'s Surfaces section) — a filter is as much a fact
  about what's served as the source type or provenance string is.

If a manifest-backed approach (Q2's (d)) is ever adopted, its own
provenance question — "as of when was this manifest built, from how many
pages" — should follow the same precedent `GCSProvenance`/`URLProvenance`
(`internal/contentsource/gcs.go`) set: a checked, reportable fact, not a
bare label. That's future work, not needed for (b)/(c).

## Q5 — Tags: any-vs-all match semantics

**Recommendation: ANY-of (a page matches if it has at least one of the
listed tags), by default and without an "all" mode initially.**

Rationale: `Frontmatter.Tags` (`internal/kb/content.go`) is used
throughout this codebase as an additive, broad categorization — a page
can carry several tags describing different facets of it (audience,
lifecycle stage, subsystem, ...), not a set of required co-conditions.
An operator writing `index_filter: { tags: [tier-1, customer-facing] }`
is overwhelmingly more likely to mean "include anything relevant to
*either* facet" (a broadening query, pulling several tag-based slices
into one filtered index) than "include only pages that carry *both*
tags simultaneously" (a narrow intersection, which is also easy to get
by a different route: combine a single-tag filter with the other
already-AND'd fields, e.g. `tags: [tier-1], category: policies`). This
also matches the *within-a-field* OR semantics recommended for every
other list-valued field in Q3 (`category: [a, b]` already means "a OR
b") — `tags` behaving the same way keeps one mental model across the
whole `IndexFilter`, rather than `tags` being the one field that means
AND while everything else means OR.

If a real deployment later needs strict intersection, add a distinct
`tags_all:` key rather than overloading `tags:`'s meaning based on some
other flag — there's no evidence of that need yet (YAGNI), and an
explicit second key is unambiguous at the call site in a way a boolean
mode flag next to a list is not.

## Recommended implementation plan

Sized against Q2's measurements: **implement (b) first, and stop there
unless a specific deployment's numbers say otherwise.** (b) is a few
lines of code reusing what already exists, captures ~90%+ of the
available speed/memory win at every corpus size measured, and every
subsequent step is opt-in complexity gated on evidence, not spec-work
done speculatively ahead of it.

1. **Add `IndexFilter` to `contentsource.Source`**
   (`internal/contentsource/config.go`, next to the existing `Layout`
   field), `stringOrList`-typed fields per Q3, validated in
   `Source.validate` — e.g. reject a filter naming no valid field,
   keeping the same "fail fast on config errors" posture the rest of
   `validate` already has for GCS/URL fields.
2. **Add match-building in `internal/kb`**, next to the existing
   `ByCategory`/`ByStatus`/`ByOwner`/`ByPrefix`/`ByType` presets
   (`internal/kb/content.go`): a `ByAnyTag(tags []string)
   FilterFunc` (Q5) and a small composer,
   e.g. `func FromIndexFilter(f contentsource.IndexFilter) FilterFunc`,
   that ANDs together whichever of the existing presets the filter
   configures. This is genuinely new code, but it's a combinator over
   presets that already exist and are already tested — not a new parser.
3. **Apply it in `internal/collections.Collection`**
   (`internal/collections/registry.go`): give `Collection` an
   `indexFilter kb.FilterFunc` (built once from `c.Source.IndexFilter` in
   `Open`), and apply it in **both** `Pages()` and `Index()` via
   `kb.Filter` — per Q1's Option 2. `Load` is untouched, which is the
   point.
4. **Surface it in provenance** per Q4: `Filter` on `collectionInfo`
   (`internal/cli/version.go`) and on `mk list --collections`'s `entry`
   (`internal/cli/list.go`).
5. **Stop.** This is approach (b) end-to-end: `kb.ListFS` still reads
   every file, `kb.Filter` narrows before `search.NewFromPages`.  Do not
   build (c)'s frontmatter-only scan or (d)'s manifest ahead of a
   concrete deployment that needs them — see thresholds below.

**When do (c)/(d) earn their complexity?**

- **(c), frontmatter-only scanning:** worth it once the O(N) file-open/
  parse pass itself becomes the visible cost, which Q2's numbers put
  somewhere **past the tens of thousands of pages on local/`type: local`
  disk** (at 50k it's still only ~4.3s out of ~10s for (b) overall — real
  but not yet the dominant term) — **or immediately, regardless of page
  count, for `type: gcs`/`type: url` collections**, where every file
  open is a network call `BenchmarkListOnly` cannot model and where
  avoiding it is a latency and cost win even at moderate scale (per Q2
  point 5). Implementation would replace step 3's `kb.ListFS` call, for
  a filtered collection only, with something like this assessment's
  `listFrontmatterFiltered` (`internal/indexfilter/corpus_test.go`) —
  promoted into `internal/kb` as real, tested production code rather
  than benchmark scaffolding, since production callers can't share
  test-only helpers.
- **(d), a precomputed manifest:** worth it only past a scale where even
  a frontmatter-only *scan* of the full corpus (Q2's "manifest build"
  row: ~3.2s at 50k, scaling with total corpus size, not filtered size)
  is itself unacceptable on the request/startup path — i.e. corpora
  large enough, or object-storage-latency-bound enough, that "walk
  everything once, cheaply" is still too expensive to do synchronously at
  every process start. That implies hundreds of thousands of pages at
  least, or a `type: gcs` prefix wide enough that even a `list`-only
  bucket walk is slow. Nothing measured here reaches that threshold; this
  assessment recommends treating (d) as a documented option, not a plan,
  until a real deployment's numbers ask for it. It also has a real
  operational cost (c)/(b) don't: something has to build and refresh the
  manifest (candidate: `mk ingest`, or the "per-collection refresh"
  follow-up multi-collection.md already tracks) — new moving parts a
  small deployment shouldn't be made to carry for a problem it doesn't
  have.

## Testing strategy (for whichever step above gets implemented)

- **Config:** `IndexFilter` YAML parse/validate (scalar-vs-list shorthand,
  unknown/empty filter, `extra.*` key decoding), on both the `content:`
  and `collections:` shapes — mirroring how `multi-collection.md`'s own
  Testing section covers both shapes for `Layout`.
- **kb:** `FromIndexFilter` composition (AND across fields, OR within a
  list field, `ByAnyTag`'s any-match), table-driven against the existing
  `ByCategory`/`ByStatus`/... tests' style.
- **collections:** a filtered `Collection`'s `Pages()`/`Index()` agree
  with each other and exclude what the filter excludes; `Load` on an
  excluded ID still succeeds (Q1's Option 2, asserted explicitly so a
  future change can't silently turn this into a security control by
  accident); `Registry.Show`/`Registry.Search` behave unchanged (Q1's
  Registry-interaction section) with a filtered collection mounted
  alongside an unfiltered one.
- **Provenance:** `mk version --json` and `mk list --collections --json`
  report `Filter` for a filtered collection and omit it for an
  unfiltered one.
- **Benchmarks:** keep `internal/indexfilter`'s harness (or promote parts
  of it, per the (c)/(d) plan above) as the regression check for "does
  filtering still bound the cost that matters" — the same spirit as
  `docs/SEARCH.md`'s existing `BenchmarkNew`/`BenchmarkQuery` regression
  target, extended to corpus sizes the current embedded-KB-sized
  benchmarks don't exercise.

## Open questions / follow-ups

- **Filtering on `Extra` beyond one dotted level** (`extra.a.b: x`): not
  designed here; revisit if a deployment's frontmatter nests data under
  `extra` deeply enough to need it.
- **Whether `mk list --collections`' reported page *count* should be the
  filtered or raw corpus size** for a filtered collection: this
  assessment assumes filtered (consistent with Q1's Option 2 — `Pages()`
  already returns the filtered set), but a raw "would index N of M
  pages" count is also plausibly useful for an operator sanity-checking
  their filter and isn't precluded by anything here.
- **Per-collection refresh** (multi-collection.md's own follow-up) and
  a manifest (Q2's (d)) are the same underlying need — a way to know
  about corpus changes without re-scanning everything — and should be
  designed together if either is ever picked up, rather than twice.
