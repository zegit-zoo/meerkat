# Lazy mounting and the resident cache

meerkat-mob issue E (#5). Issue D (#4) made a `mount: lazy` child of the
tree a declared-but-unmounted name. This page makes it a knowledge base
that is mounted on first request and kept resident under a budget, with
the neural-plasticity model from the brief: paths traversed often stay
resident, rarely traversed KBs live only in their store, and the
process stays under a ceiling by culling the least-travelled subtrees
from the cache — never from content.

## Cold collections

A lazy child is a `Collection` from startup: named, placed in the tree,
carrying its source, and **cold** — no snapshot, no pages, no index. A
cold collection is listed (`mounted: false`, `source: cold`, zero
pages) and is ready (`/readyz` has nothing to be unready about); it is
never in the refresh schedule and never falls through to the embedded
build. Listing it does not mount it.

The first request that names it — `mk_search`, `mk_show`, `mk_list`
with `collection`, or a tree path — mounts it: resolve the source (the
same cache, conditional reads and guards a `collections:` entry gets),
mount the layout, enumerate pages, build the index, install the
snapshot. The mount is serialised per collection; a second caller waits
and finds it warm. The `meerkat.mount` span carries the trigger (lazy |
eager | warmstart), the tier and the byte estimate; `meerkat_mount_total`
and `meerkat_mount_duration_seconds{tier}` count and time it.

## Cold policy

```yaml
cache:
  cold_policy: blocking     # blocking (default) | async
  retry_after: 2s
```

`blocking` mounts inside the request and answers. `async` starts the
mount detached from the request and answers at once with

```json
{"status": "cold", "collection": "vendors", "retry_after_ms": 2000, "message": "…"}
```

— a tool result, not an error: the caller did nothing wrong. The same
call retried after `retry_after` finds the collection warm. The HTTP
and CLI surfaces see the blocking behaviour through the registry.

## The budget

```yaml
cache:
  max_bytes: 64MiB          # 0 = unbounded (the default)
  high_watermark: 0.65
```

Resident bytes are the sum over lazily mounted collections of
`search.Index.SizeEstimate()`: page bytes (title + body) plus what
bleve reports for its segments, or twice the page bytes when it reports
nothing. An estimate, deliberately — the budget needs a number that
moves with content size, not an exact one, and Q5's answer is that the
real control loop is the metrics (`meerkat_cache_resident_bytes`,
`meerkat_cache_fill_ratio`, `meerkat_collections_resident`,
`meerkat_mount_duration_seconds{tier}`).

Eager collections and the root are never counted and never culled.

## Culling

Every traversal of a collection (a search that reached it, a page
shown from it) raises its **temperature**; the first traversal stamps a
**first-traversal sequence**. When a mount pushes the cache over
`high_watermark × max_bytes`, resident lazy collections are culled —
lowest temperature first, then oldest first traversal — until it is
below the watermark. `meerkat_cache_cull_total{reason}` says which rule
chose (`temperature`, `age`) or that an operator evicted (`evict`,
`Registry.Evict`).

A culled collection goes back to cold: the empty snapshot is installed,
readers holding the old one drain, then its index closes. Nothing is
deleted — the next request mounts it again. This is Q8: culling
applies to the cache only; content lifecycle is the librarian's (issue
H).

The collection a request just mounted is never culled by its own mount:
the request needs it now, even if that evicts a warmer one. Warm start
is the exception (below).

## Temperatures and warm start

```yaml
cache:
  flush_interval: 5m
  warm_start_days: 7
observability:
  traversal_log: {…}        # required for both
```

Every `flush_interval` (and at shutdown) the registry writes a
temperature record to the traversal log (issue G): `{kind:
temperature, entries: {<hmac(name)>: {temperature, first_traversal,
tier}}}`, one object per flush, names hashed like everything else in
that log. `meerkat_path_temperature` observes the distribution at each
flush.

At startup, with `warm_start_days > 0`, the registry reads the last N
days of flushes, keeps the latest counters per hashed name, matches
them to its cold collections by hashing their names with the same key,
restores the counters, and mounts the hottest first until the next
mount would cross the watermark — that one is unmounted again and the
warm start stops. A restart after an hour of traffic therefore comes up
with the most travelled paths already resident.

## Promotion

Promotion of a very hot deep path toward tier 0 is proposed by the
librarian from the temperature data and lands through the update
contract; the cache never rewrites content. The temperature records are
the input; nothing here moves a page. The pass itself — top-N hot
collections at depth ≥ 2 without a root pointer, filed as a pointer
page through the root's contract — is described in
[intake.md](intake.md#the-librarian).

## Seams

| What | Where |
|---|---|
| config | `contentsource.CacheSpec` (`cache:` block) |
| cold collection state, mount, cull, flush, warm start | `internal/collections/cache.go` |
| lazy child source carried for later | `contentsource.TreeNode.LazySource` |
| size estimate | `search.Index.SizeEstimate` |
| temperature records | `traversal.Log.RecordTemperatures` / `ReadTemperatures` |
| cold answer on MCP | `coldResult` in `internal/mcp/server.go` |
