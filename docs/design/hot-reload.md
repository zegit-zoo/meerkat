# Spec: Runtime reconciliation — hot-reload GCS collections and memory

**Status:** Implemented (`internal/refresh`, `internal/collections` reload path, `internal/contentsource` probe, `internal/memory` fingerprint; wired into `mk mcp serve-http`) · **Builds on:** [multi-collection.md](multi-collection.md), [memory.md](memory.md), [hosted-mcp.md](hosted-mcp.md) · **Issue:** #28

*(`docs/design/` is a design record. The up-to-date schema reference is
[content-source.example.yaml](../../content-source.example.yaml); the
user-facing reference is the [README](../../README.md).)*

## Summary

`type: gcs` already resolves content correctly: object generations,
`ifGenerationMatch` preconditions, hardened extraction, generation- and
fingerprint-keyed caches. It resolves it **once, at process start**.

That leaves two operational gaps for a hosted deployment:

1. A publication pipeline writes a new approved generation to the
   bucket, and meerkat keeps serving the old one until somebody rolls the
   process.
2. Several replicas share a GCS memory store. A memory written through
   replica A is indexed by A immediately and is invisible through replica
   B until B restarts.

This spec adds an **opt-in polling reconciliation controller**. Object
storage stays authoritative; readers converge on their own, without
dropping a query.

The whole design is shaped by one constraint, the same one every other
spec in this directory is shaped by: **a deployment that does not opt in
behaves exactly as it did before.** No `refresh:` block means resolve
once, never poll, never swap — the immutable-deployment behaviour, and
still the default.

## Non-goals

- **Watching arbitrary local filesystems.** `type: local` already
  re-enumerates per request; a file watcher is a different problem with
  different failure modes.
- **GCS notifications / Pub/Sub.** Polling metadata is portable, needs no
  extra IAM surface, no topic, no subscription, and no inbound path into
  the process. It is sufficient at the cadences a knowledge base
  publishes at.
- **Replacing Git/MR publication or corpus validation.** meerkat reads
  what the pipeline published; it does not review it.
- **Writing to GCS content.** `type: gcs` content is still read-only.
  (The memory store is the writable surface, and always was.)

## Config schema

```yaml
collections:
  - name: handbook
    type: gcs
    bucket: example-kb
    prefix: handbook/live/
    refresh:
      interval: 60s              # required, >= 5s
      jitter: 10s                # optional, < interval
      failure_policy: serve-last-good   # serve-last-good (default) | unready
    memory:
      type: gcs
      bucket: example-kb
      prefix: handbook/memory/
      refresh:
        interval: 15s            # same block, same rules
```

`refresh:` sits on the source (content) and, independently, inside
`memory:` (the writable store). They are separate targets with separate
schedules, separate status and separate metrics, because they answer to
different clocks: a knowledge base is republished occasionally, a memory
is written mid-conversation.

**Durations must carry a unit.** `interval: 60` is an error, not sixty
seconds and emphatically not the sixty *nanoseconds* a raw
`time.Duration` field would have parsed it as.

**`interval` has a 5s floor.** The bound is about the bucket's metadata
quota, not meerkat's cost. An operator who needs a change live *now* has
the admin trigger.

**`jitter` is additive**, uniform in `[0, jitter)`, drawn independently
per cycle. Replicas of a hosted service start together; without jitter
they probe together, and — worse — all download the same new generation
at the same instant, so one publication costs N simultaneous fetches.

### Two refusals

Both are refusals at config load, not warnings, because in each case the
tolerant reading is the dangerous one.

**`refresh:` is `type: gcs` only.** `type: local` is already live,
`type: url` is pinned by a mandatory digest that cannot move without the
config moving, and watching local filesystems is a non-goal. A `refresh:`
block on any of them is a misunderstanding worth naming.

**`generation:` and `refresh:` are mutually exclusive.** This is the
security-relevant one. Pinning a generation means *serve exactly these
bytes until the configuration changes* — it is how a deployment becomes
reproducible, and how an operator guarantees that a later bucket write
cannot alter what is served. Accepting a `refresh:` block beside it would
leave two readings of one file ("the pin wins, refresh is dead config"
vs. "refresh wins, the pin is advisory"), and one of those silently
revokes the guarantee. Refusing the pair means a pinned source can never
start moving by accident. `Source.Refreshable()` re-asserts the same rule
at runtime, so the failure direction stays closed even if validation is
ever bypassed: the worst outcome is a source that does not move.

**`memory.refresh` is `type: gcs` only** for a different reason: a local
store is a directory one process owns. There is no second writer to
converge with, so a poll loop could only ever re-read what this process
itself wrote.

## Reconciliation model

Every cycle, content or memory, is the same four steps.

| step | content | memory |
| --- | --- | --- |
| 1. probe (metadata only) | object generation, or sha256 over the prefix listing's sorted `(name, generation)` pairs | sha256 over the live documents' sorted `(name, generation)` pairs |
| 2. unchanged? | stop | stop |
| 3. resolve, off the request path | `FetchGCS` — the same hardened path startup uses | `Store.Load` — the same call `AttachMemory` uses |
| 4. rebuild + swap | new index over the new content root + the live overlay | new index over the live content root + the new overlay |

Step 2 is the point. The overwhelmingly common outcome of a poll is
"nothing changed", and it costs exactly one metadata call: no download,
no parse, no reindex, no cache write.

The probe's token is byte-identical to the one `FetchGCS` keys its cache
on (`contentsource.GCSVersion` and `FetchGCS` share the fingerprint and
the filtering), which is what makes the comparison in step 2 meaningful
rather than approximate.

Step 3 re-reads the current generation itself rather than being handed
the probe's answer. A publication that landed between the two is then
resolved coherently as the newer generation, with its own conditional
reads, instead of being fetched under a version token that no longer
describes it.

### The snapshot, and the swap

A collection's servable state is a **snapshot**: the filesystem its pages
are read from, the provenance string naming exactly which bytes those
are, the version token, and the search index built over them. All of it
is replaced together or not at all — which is what "never expose a
partially downloaded, partially parsed or partially indexed collection"
means concretely. There is no instant at which the filesystem is new and
the index is old.

Swapping a `*search.Index` out from under a running query is a **new**
hazard this change introduces, and it is worth naming precisely because
it is *not* a data race — bleve is concurrency-safe, and the race
detector would see nothing. It is a **use-after-close**: a query that has
taken the index off the collection and is about to call `QueryAs` gets
"index closed" if a refresh closed it in between. In production that is a
burst of failed searches every time a new generation lands.

So the snapshot is reference-counted:

```
acquire()   under snapMu.RLock: read the pointer AND take a reference,
            indivisibly
install()   under snapMu.Lock: publish the new snapshot, then drop the
            collection's own reference to the old one
release()   at zero: close the index
```

Because `acquire`'s read-and-increment happens under the read lock and
`install`'s swap under the write lock, the two cannot interleave: every
reader that obtained a pointer obtained a reference with it. The old
index is closed only after the last in-flight reader releases it.
In-flight requests run to completion against the generation they started
on; requests arriving after the swap see the new one.

`Collection.Index()` deliberately does **not** hold a reference. It is
for warming an index at startup and for asking whether one builds at all
(readiness) — never for running a query. Everything that actually uses an
index goes through `searchAs` or `indexLiveWrite`, which hold one for the
duration.

### The other race: a memory write during a rebuild

Building the replacement index takes time, deliberately off the request
path. A memory saved during that window would be indexed into the *old*
index and be absent from the *new* one — a write the caller watched
succeed, silently lost one swap later.

The fix is a staging journal:

- `beginStaging` arms `pending` before the rebuild starts;
- every live index write (`SaveMemory` → `indexLiveWrite`) records itself
  in `pending` **and** indexes normally, both under `writeMu`;
- `commit`, under the same `writeMu`, replays `pending` into the new
  index — and into the rebuilt overlay, for a memory reload — *before*
  publishing it.

The lock makes the ordering total: a write either lands before the replay
and is carried across, or entirely after the commit and goes straight
into the new snapshot. There is no third case. A failed commit costs
nothing, because a journalled write is already in the live index.

### One reload slot per collection

`reloadMu.TryLock()` guards a whole cycle. A second cycle — a slow
refresh overrunning its interval, an admin reload arriving mid-poll, or
the *memory* target for a collection whose *content* target is already
staging — gets `refresh.ErrBusy` rather than a second concurrent swap.
`ErrBusy` is counted separately from a failure and degrades nothing: it
is the system working.

### Memory reconciliation and the #27 line

The rebuilt overlay is constructed from `memory.Page(rec.Key, rec.Body)`
— the store's own key — and never from what a document's frontmatter
claims about itself. A personal memory's owner is derived from its page
ID, the page ID from the store key, and the store key from the verified
identity that wrote it. Reconstructing an ID from a `memory_namespace:`
field would let a document's bytes choose whose memory it is.

The index is built over the **unfiltered** page set, exactly as the
mount-time build is. Every document is in the index, including private
personal memories; visibility is a clause in the query
(`internal/search`'s `visibilityClause`), applied at read time. A
document filtered out at index time would be invisible to its own owner,
permanently, with no error anywhere.

`Collection.personalReadsAreCollectionWide` is untouched by a reload: a
refresh swaps the snapshot *inside* the `*Collection` rather than
replacing the `*Collection`, so every field outside the snapshot keeps
its mount-time value.

`TestViewedBy_SurvivesARemount` now runs the whole property twice — over
a fresh mount, and over a live collection that reconciled while serving.

### What a failure leaves behind

Nothing. That is the contract:

- the previous snapshot is still installed and still serving;
- the last known-good cache entry is untouched — a new generation
  populates a cache directory keyed by the *new* version, through the
  existing staging-directory-plus-atomic-rename finalisation, so a failed
  fetch cannot delete or corrupt the entry currently in use;
- the overlay is not emptied by a failed memory `Load`;
- the collection is marked **degraded**, which is reported.

`failure_policy` chooses what degraded does to readiness:

| policy | serving? | `/readyz` | when |
| --- | --- | --- | --- |
| `serve-last-good` (default) | yes | 200, `status: degraded` | almost always. A pipeline pushing a broken generation, or a transient 503, must not drain a fleet of otherwise-healthy replicas — usually *every* replica, since they read the same bucket. Serving yesterday's approved knowledge base beats serving none. |
| `unready` | yes | 503 | when stale content is a correctness problem rather than an inconvenience. The replica still answers the requests it has: failing readiness is not refusing to serve. |

## Observability

### `/readyz`

Counts and state only, unchanged in posture — it is unauthenticated, and
which collections a deployment mounts is not public information.

```json
{"status":"degraded","collections":{"ready":3,"degraded":1,"total":3}}
```

Two axes, deliberately separate. **Ready** means the collection
enumerates and holds a built index — it is answering queries, and it
drives the HTTP status. **Degraded** means its last refresh failed and it
is serving the last known-good snapshot. A degraded collection is
normally still ready, so `status: degraded` with HTTP 200 is a real and
useful state: something is worth looking at, nothing is down. No name, no
bucket, no generation and no error string appears in the body; those go
to the structured log and to authenticated collection discovery.

### Metrics

All labelled by the collection's configuration **ordinal** and the target
**kind** (`content` | `memory`), and by nothing else:

```
meerkat_refresh_attempts_total{collection,kind}
meerkat_refresh_changes_total{collection,kind}
meerkat_refresh_failures_total{collection,kind}
meerkat_refresh_skipped_total{collection,kind}          # overlap suppressed
meerkat_refresh_duration_seconds{collection,kind}
meerkat_refresh_last_success_timestamp_seconds{collection,kind}
meerkat_refresh_degraded{collection,kind}               # 0/1
meerkat_collections_ready
meerkat_collections_degraded
```

Every series is published as a zero at startup, so a target that has
never failed reports `0` rather than nothing at all — "no data" and "no
failures" look identical to a naive alert, and only one of them is true.

**A collection name, bucket, object path, prefix, memory key or principal
is never a label.** `/metrics` is unauthenticated, so a label is as
public as the endpoint. **A generation or fingerprint is never a label
either**, for a second reason: it increments forever, so one series per
publication is an unbounded cardinality leak that on a shared Prometheus
takes other tenants down with it. The version travels in the structured
status (`Collection.ReloadStatuses`) and in the log line, where it is
useful and bounded.

An ordinal maps back to a name through the configuration the operator
already has, and the log line already carries the name.

### Structured status

The detail the probes and the metrics deliberately omit lives on the
**authenticated collection-discovery surfaces** — `mk_list_collections`
and `GET /collections` — as a `refresh` array, one entry per configured
target, absent entirely for a collection that is resolved once:

```json
{
  "kind": "content",
  "interval": "1m0s",
  "failure_policy": "serve-last-good",
  "version": "1748112233445566",
  "last_attempt": "2026-08-24T09:14:02Z",
  "last_success": "2026-08-24T09:13:02Z",
  "degraded": true,
  "error": "collection \"handbook\": probe example-kb: ..."
}
```

That is the right home for it: those surfaces are already gated, and
already narrowed to the collections the caller may read, so a generation
and an error string disclose nothing the caller could not already see.
`last_success` deliberately does **not** move on a failed cycle — "it
last worked at T" is the number that says how stale the content actually
is.

### Logging

One line per applied change (`collection`, `kind`, `version`), one per
failure (with the policy and the error), and a debug line for a skipped
overlap. A no-change probe logs nothing: at a 15s interval it would
otherwise be 5,760 lines a day per target saying "no".

## The admin trigger

`SIGHUP` runs one cycle for every configured target, immediately, through
`HostedServer.Reload` → `Controller.ReloadNow` → the same
`Target.Reconcile` the scheduled loops call. There is deliberately no
second update path: it would be a second place to get the staging
discipline, the generation preconditions and the atomic swap wrong.

A signal rather than an HTTP endpoint. An endpoint would be a new
*mutating* surface that has to be authenticated — the operational
endpoints beside it are all unauthenticated by design, and a reload
trigger emphatically cannot join them — rate-limited (it can be made to
hammer a bucket), and reasoned about for every deployment topology. A
signal is authorized by the operating system: you can send it if you can
already signal the process, which is strictly less access than being able
to restart it, the thing this feature exists to avoid needing.

It cannot race a scheduled cycle: the collection's reload slot refuses
the second caller.

## Security and reliability properties

- **ADC/WIF only.** The probe constructs the same credential-optionless
  client the fetch does. There is still no field anywhere in the schema
  for a static service-account key.
- **Generation preconditions preserved.** Reconciliation calls `FetchGCS`
  unchanged: explicit generation *and* `ifGenerationMatch` on every read.
  The probe is metadata-only and never a substitute for them.
- **Bounded by the existing source limits.** `maxGCSObjects`, the
  per-file and cumulative byte caps, and `maxMemoryObjects` all apply to
  a refresh exactly as they apply to startup. The probe enforces the
  object-count cap too, so a mistyped prefix fails at the cheap step
  rather than hashing a whole bucket every minute forever.
- **No overlapping refreshes per collection**, and none across the
  content/memory pair of one collection.
- **No unbounded retained snapshots.** Exactly one snapshot is installed;
  the previous one is released at the swap and its index closed once its
  last reader drains. A prepared-but-not-installed snapshot (a failed
  commit) is discarded explicitly rather than left to the garbage
  collector, which would never close its index.
- **A failed refresh never corrupts the last-good cache.**
- **The staged/pending area is excluded from the memory fingerprint**, by
  sharing one `liveDocumentKey` decision with `Load`. Writing a proposal
  therefore neither publishes it nor triggers a fleet-wide reload.

## Rollout-free refresh vs. pinned deployment

Two deliberate, opposite postures. Both are supported; neither is
implicitly upgraded into the other.

| | pinned (`generation:`) | refreshed (`refresh:`) |
| --- | --- | --- |
| what is served | exactly the configured generation, forever | whatever the bucket currently holds |
| changing it | edit config, redeploy | publish to the bucket |
| metadata calls | none, ever | one per interval per target |
| reproducible | yes — two replicas started a month apart serve identical bytes | eventually consistent within one interval (plus jitter) |
| use it for | audited/regulated corpora, release-pinned deployments, a rollback you can name | a handbook that should be current, replicas sharing a memory store |

A `prefix:` source with no `refresh:` block sits between the two: it
resolves whatever the prefix held at startup and then never looks again,
which is reproducible only for the lifetime of the process. If that is
the intent, say so with `generation:` (object mode) — and if it is not,
say so with `refresh:`.

## Testing strategy

- **Config:** durations with and without units, the interval floor, the
  jitter bound, the policy enum, and each of the three refusals (pinned +
  refresh, refresh on a non-gcs source, memory refresh on a local store),
  each naming the offending config path.
- **Probe (`internal/contentsource`, against the existing fake):** the
  probe's token agrees with `FetchGCS`'s at every step, for object mode
  and for prefix add/overwrite/**delete**; a pinned generation makes no
  metadata call; the object-count cap refuses; the probe is silent about
  unsafe object names while the fetch still warns once.
- **Fingerprint (`internal/memory`, against the existing fake):** changes
  if and only if `Load`'s result would — created, overwritten, and *not*
  for a staged proposal or a non-markdown object; two stores over one
  bucket converge and agree.
- **Reconciliation (`internal/collections`):** a new generation becomes
  searchable with no restart; add+delete lands as one snapshot; an
  unchanged probe resolves nothing and rebuilds nothing; a failed resolve
  and a failed probe both leave the last known-good content serving and
  mark the collection degraded (ready under `serve-last-good`, not ready
  under `unready`); overlapping cycles get `ErrBusy`; a memory write
  during a rebuild survives the swap; a failed memory load does not empty
  the overlay.
- **Replica convergence:** two registries over one shared fake bucket,
  converging in both directions — including that alice's personal memory
  becomes readable by *alice* on the other replica and by nobody else.
- **Race:** snapshots swapped in a loop under concurrent search, show,
  list and memory writes as three principals, plus a memory reconcile
  racing the content reconciles. It checks two different failures: a data
  race (the detector's job) and a use-after-close (which surfaces as a
  query error). Afterwards, every write that was reported as saved is
  still findable, and still invisible to the other principals.
- **Hosted surface:** refresh is off unless configured and publishes no
  metrics then; a failed cycle produces `status: degraded` with HTTP 200,
  the documented counts, and the documented series — with no collection
  name, bucket or prefix anywhere in either; `unready` produces a 503;
  and the freshness detail those two omit does reach authenticated
  collection discovery, with `last_success` left untouched by the
  failure.

## Follow-ups

- **Incremental memory reload.** A changed fingerprint currently
  re-reads every live document. The listing already carries per-object
  generations, so re-reading only the objects whose generation moved is a
  contained improvement; it needs a `Store` method that takes a previous
  listing.
- **A replica's own writes move the fingerprint**, so the next probe
  re-reads a store this process is already current with. Harmless and
  bounded by the interval, and the incremental reload above removes most
  of its cost.
- **Other object stores.** The probe is the same shape for an S3 ETag or
  an Azure Blob ETag; `Fingerprinter` and `GCSVersion` are the two seams.
- **Per-collection refresh for `type: local`**, if a file watcher ever
  becomes worth its failure modes. The reconciliation half is already
  backend-agnostic — only the probe is GCS-shaped.
