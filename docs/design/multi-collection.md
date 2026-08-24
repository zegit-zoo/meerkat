# Spec: Multi-collection runtime + GCS content backend

**Status:** Implemented (`internal/collections`, `internal/contentsource` `type: gcs`; wired into CLI, MCP and HTTP) · **Builds on:** [content-sources.md](content-sources.md) · **Issue:** #8

*(`docs/design/` is a design record. The up-to-date schema reference is
[content-source.example.yaml](../../content-source.example.yaml); the
user-facing reference is the [README](../../README.md).)*

## Summary

meerkat resolved exactly one content source at runtime: a `--kb-dir`, a
single-source `content-source.yaml`, or the build-time embed. One binary,
one knowledge base.

Organizations keep more than one. Domain docs, runbooks, architecture
records and team notes have different owners, different refresh cadences,
different sensitivity, and — the reason this issue comes first in its
series — different access rules. This spec mounts several **named
collections** in one process, adds **Google Cloud Storage** as a
first-class runtime backend, and routes search/show/list across them.

The whole design is shaped by one constraint: **a single collection must
behave exactly as meerkat did before collections existed.** Every
configuration that predates this change resolves to one collection named
`default`, and in that state nothing is qualified, nothing is
disambiguated, and `mk version` prints the same `kb_source` string it
always did.

## Non-goals

- **Per-collection authorization.** The next issue in the series. This
  spec establishes the abstraction it needs (a stable, validated
  collection name; a registry that resolves a request to a specific
  collection before any content is read) but grants no per-collection
  access control of its own: a mounted collection is readable by anyone
  who can reach the process.
- **Collection-aware ingestion.** `mk ingest`, the ingestion source
  registry (`internal/sources`) and shell completion still read one
  content root — the *first* configured collection. Generalizing them is
  its own piece of work.
- **Merging collections into one index.** Each collection keeps its own
  bleve index (see "Cross-collection ranking").
- **Writing to GCS.** `type: gcs` is read-only.

## Config schema

A `content-source.yaml` has one of two shapes. Setting both is an error,
not a merge — a reader should never have to work out which of two content
declarations won.

```yaml
# (a) single source — the original schema, byte-for-byte unchanged.
content:
  type: local
  path: ../meerkat-kb

# (b) several named sources, all mounted at once.
collections:
  - name: runbooks
    type: local
    path: ../runbooks-kb
  - name: architecture
    type: gcs
    bucket: my-org-knowledge
    object: bundles/architecture-v3.tar.gz
  - name: team-notes
    type: gcs
    bucket: my-org-knowledge
    prefix: notes/live/
  - name: vendor-docs
    type: url
    url: https://example.com/kb/vendor-v2.tar.gz
    sha256: "e3b0…"
    layout: { wiki: docs }
```

Each `collections:` entry is a `Source` (the same struct `content:`
parses, inlined) plus a `name`. Every source key — including a
per-collection `layout:` — works identically in both shapes.

**A list, not a map.** YAML mappings have no defined order, and order is
load-bearing here: it is the order collections are listed in, searched
in, and disambiguated in, and it selects the collection that backs the
not-yet-collection-aware surfaces. A list makes that order explicit and
diff-stable.

**Name grammar:** `[A-Za-z0-9][A-Za-z0-9_-]*`, max 64 characters, unique.
No colon, because a name is also the `<collection>:` prefix of a
qualified page ID. The constraint is deliberately tighter than it needs
to be today: with per-collection authorization, a name becomes a
principal in a policy document, and widening a grammar later is easy
where narrowing it is not.

`type: none` is rejected for a named collection: an unserveable
collection is a configuration mistake, not an empty-KB fallback. That
fallback exists only for the single-source form.

## Registry and routing

`internal/collections.Registry` owns the mounted collections. Each holds
its content root (an `fs.FS` over a resolved directory, via
`internal/kbdir`'s adapter and that collection's own layout) and, built
lazily and once, its search index. Every surface — CLI, MCP, HTTP —
routes through the same methods, so there is exactly one place the rules
live.

| request | `collection` given | `collection` omitted |
| --- | --- | --- |
| search | that one | all, merged by score, truncated to `limit` |
| list | that one | all, in configuration order |
| show | that one | all in configuration order; **one** match returned, **several** is an error |

An unknown name is an error naming the mounted ones (`ErrUnknownCollection`).

### Page IDs stay page IDs

A page is addressable as `<collection>:<page-id>` anywhere a page ID is
accepted. The `<collection>:` prefix is recognised only when it names a
*mounted* collection, so an ID that happens to contain a colon is never
mistaken for a qualification.

Machine-readable output never rewrites an ID. `mk … --json`, the MCP
tools and the HTTP endpoints all report the page's own unqualified `id`
plus a separate `collection` field. Anything that round-trips an ID (a
link, a bookmark, `mk ingest`) is therefore unaffected by how many
collections a deployment mounts. Only *human-readable* CLI output prints
the qualified form, and only when more than one collection is mounted —
so it can be pasted straight back into `mk show`.

### Ambiguity is an error, not a pick

`mk show shared/overview` with that ID in two collections fails, listing
`runbooks:shared/overview` and `architecture:shared/overview`. It does
not return the first.

Silently preferring the first collection would mean the answer to "show
me this page" depends on config-file ordering the caller can't see — and,
once per-collection authorization lands, that a caller's *permissions*
silently change which document they get. Failing with the two qualified
IDs costs one extra round-trip and is always unambiguous. HTTP maps this
to **409 Conflict** (the page exists, more than once — distinct from 404
and from 400); MCP returns it as a tool-level error the model can retry
with, not a transport failure.

### Cross-collection ranking

Each collection is queried independently with the same `limit`, and the
union is re-sorted by score. Scores are BM25 values from independent
indexes, so they are comparable only approximately. The alternative — one
shared index over every collection — would rank better but forfeits the
per-collection isolation this abstraction exists to provide: it could not
be filtered per caller without rebuilding, which is precisely what
per-collection authorization needs. Ties break on configuration order,
then page ID, so output is deterministic.

Since #27 each per-collection query also carries a per-caller
**visibility clause** (see [memory.md](memory.md#private-personal-reads-27)),
so a caller's `limit` is spent on documents they may actually see rather
than on personal memories that would be discarded afterwards. That is a
clause in the query, not a second index: the "one index per collection,
filtered per caller by narrowing the registry" arrangement above is
unchanged, and the clause is what extends it from *which collections*
to *which pages within them*.

### The single-collection fallback

`internal/kb` reads through process-global state (`kb.UseFS`), which
`internal/sources`, `mk ingest` and completion all rely on. Rather than
tear that out, a **one-collection registry keeps a nil filesystem** and
reads through the globals — exactly the code path that ran before. A
collection only gets its own `fs.FS` when several are mounted. Two
consequences, both deliberate:

- Every pre-existing configuration, and every subcommand executed
  without a root command (as this repo's own tests do), behaves
  identically.
- With several mounted, the globals point at the **first** collection,
  which is what the not-yet-collection-aware surfaces read. Documented,
  not accidental.

## GCS backend (`type: gcs`)

Two modes, exactly one of which must be configured:

- **`object:`** — one `.tar.gz` bundle. The GCS analogue of `type: url`.
- **`prefix:`** — every object under a prefix, mirrored as a directory
  tree with the prefix stripped.

### Immutability and caching

`type: url` is keyed (and verified) by a mandatory sha256. GCS gives a
better primitive: every write assigns a new **generation**, so
`(bucket, object, generation)` names immutable bytes just as a digest
does.

| mode | cache key | invalidated by |
| --- | --- | --- |
| `object:` | the object's generation | any overwrite (new generation) |
| `prefix:` | sha256 over the sorted listing's `(name, generation)` pairs | any add, overwrite or delete under the prefix |

Both live under the same user-cache scheme `type: url` uses
(`<user cache dir>/meerkat/content/gcs/<hash(bucket,target)>/<version>/`)
and reuse its completion-marker + atomic-rename finalisation, factored
out as `populateCacheDir`: a half-populated directory is never visible at
the cache path and never counts as a cache hit. Because generations are
immutable, previous entries stay valid — a rollback re-uses a cached one
rather than re-downloading.

Reads set **both** an explicit generation and an `ifGenerationMatch`
precondition. The generation selector asks for exactly those bytes; the
precondition makes the request *fail* rather than silently serve
something else. Together: the content in a cache entry named `<gen>`
cannot be the content of any other generation.

`generation:` may be pinned in config, which skips the metadata lookup
entirely — the reproducible-deployment equivalent of pinning a `type:
url` source by digest, and proof against a later overwrite changing what
a deployment serves. `sha256:` is optional for GCS (the generation
already pins the bytes) but is verified before extraction when set.

### Credentials

Application Default Credentials only: Workload Identity Federation, a
GKE/Cloud Run/GCE service account, an impersonated principal, or
`gcloud auth application-default login` for a developer. `storage.NewClient`
is constructed with **no** credential options, which is what keeps all of
those paths available.

**There is no key-file field in the schema.** That is the security
property, not an omission: meerkat cannot be *asked* to load a static
service-account key, so a config review has nothing to catch.

### Hardening

Remote, operator-influenced names and bytes get the same treatment the
`type: url` extraction path already had:

- Every write goes through an `os.Root` rooted at the staging directory —
  a string-joined path is not a containment boundary.
- Object names are validated with `archive.go`'s `safeEntryName` (no
  absolute paths, no `..`, no backslash or colon). An unsafe name is
  skipped with a warning rather than being fatal: one oddly-named object
  in a shared bucket must not make a whole collection unserveable.
- Trailing-slash "directory placeholder" objects are skipped.
- Caps: per-file bytes, cumulative bytes, and object count
  (`maxGCSObjects`, so a mistyped prefix that names a whole bucket fails
  loudly instead of downloading it).

### Test seam

`newGCSClient` is a package var over a small `gcsAPI` interface (Attrs /
Objects / Open / Close). Every GCS test swaps in an in-memory fake with
real generation semantics — including `ifGenerationMatch` failures. No
test anywhere needs credentials, a bucket, or the network.

## Surfaces

**CLI.** `--collection` on `search`/`show`/`list` (with shell completion
from the mounted set); `mk list --collections` enumerates name, type,
provenance and page count. `mk version` keeps `kb_source` as a single
string — the collection's own provenance when one is mounted,
`collections:<n>` when several — and adds a `collections` array with each
one's name, type and provenance, so a multi-collection deployment reports
everything it serves.

**MCP.** An optional `collection` argument on `mk_search`/`mk_show`/
`mk_list`. Discovery is folded into the tool descriptions, which name the
mounted collections: a client learns the set from the tool list it
already fetches, with no extra tool to call and nothing added to a
single-collection server's prose.

**HTTP.** An optional `collection` field on `/search`, `/show` and
`/list`; a new auth-gated `GET /collections` (which collections a
deployment mounts is not public information); the collection field and
names published in `/openapi.json`.

## Provenance vocabulary

| source | `kb_source` |
| --- | --- |
| embed | `embedded` |
| `--kb-dir`, `type: local` | `disk:<path>` |
| `type: url` | `url:<url>@<sha256[:12]>` |
| `type: gcs` (object) | `gcs://<bucket>/<object>@<generation>` |
| `type: gcs` (prefix) | `gcs://<bucket>/<prefix>*@<fingerprint>` |
| several collections | `collections:<n>` (+ the per-collection array) |

Like `url:`, the token after `@` on a `gcs:` line is a *checked* property
of what is being served (the conditional read cannot return another
generation), not a label — unlike `disk:`, which names an arbitrary,
unverified directory.

## Testing strategy

- **Config:** single-source back-compat (including that no `content.*`
  error message changed), collections parse/order/layout-merge, and every
  rejection (both shapes set, duplicate/invalid names, `type: none`,
  per-collection field errors naming the collection).
- **Registry:** all-vs-one for pages and search, merged-limit semantics,
  the ambiguity error and both ways to resolve it, qualified-ID parsing
  against non-mounted prefixes, and that `Open` gives a single collection
  the process globals but mounts each of several separately.
- **GCS:** fetch/extract/cache, generation-based invalidation, pinned
  generations skipping metadata, `ifGenerationMatch` failure, optional
  sha256 both ways, prefix tree mirroring and fingerprint invalidation,
  unsafe-name skipping, and the size/count caps — all against the fake.
- **Surfaces:** end-to-end through the real cobra tree, the MCP handlers
  and the HTTP mux, for both the multi-collection routing and the
  single-collection back-compat.

## Follow-ups

- ~~**Per-collection authorization**~~ — done, in
  [hosted-mcp.md](hosted-mcp.md) (#9). It landed slightly upstream of
  where this section predicted: rather than filtering inside
  `Registry.target`, `Registry.Restrict` narrows the whole registry
  once per request, which also covers `Names`, `All`, `Get` and
  `SplitQualified` — enumeration surfaces that don't route through
  `target` and would each have leaked on their own. The invisibility
  warning above turned out to be the load-bearing part of the design;
  `Registry.Search`'s per-collection indexes did indeed mean a filtered
  search needs no rebuild, since a restricted view shares the parent's
  `*Collection` values.
- **Collection-aware ingest / sources / completion**, replacing the
  first-collection compromise above.
- **Per-collection refresh.** Content is resolved once at startup;
  serving a new GCS generation needs a restart. A background
  re-resolve-and-swap is possible (the registry already holds each
  collection behind a pointer) but out of scope here.
- **Other object stores.** The `gcsAPI` seam is GCS-shaped but small; S3
  or Azure Blob would follow the same generation/ETag-keyed pattern.
