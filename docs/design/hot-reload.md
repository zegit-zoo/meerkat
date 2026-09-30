# Spec: Runtime reconciliation — hot-reload GCS collections and memory

**Status:** Implemented (`internal/refresh`, `internal/collections` reload path, `internal/contentsource` probe, `internal/memory` fingerprint, `internal/kb` local fingerprint; wired into `mk mcp serve-http`, and for scheduled targets into `mk mcp serve`) · **Builds on:** [multi-collection.md](multi-collection.md), [memory.md](memory.md), [hosted-mcp.md](hosted-mcp.md) · **Issue:** #28

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

- **A file watcher.** No inotify, no fsnotify, no per-request
  re-indexing. A `type: local` collection reads its pages live but builds
  its **search index** once. SIGHUP rebuilds it on demand, and a
  `refresh:` block rebuilds it on a timer when a cheap fingerprint of the
  directory moves. A timer and a token, not a watcher. See
  [SIGHUP and `type: local`](#sighup-and-type-local) and
  [Scheduled refresh for `type: local`](#scheduled-refresh-for-type-local).
  An earlier version of this list called watching local filesystems a
  non-goal on the grounds that `type: local` is already live. That was
  only half true. The pages were live and the index was not, so a
  long-running server's search disagreed with its own `mk_show`
  (meerkat-mob#25).
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

**`refresh:` is for the object stores and `type: local` only.**
`type: url` is pinned by a mandatory digest that cannot move without the
config moving, and git, submodule and embedded content is resolved at
build time. A `refresh:` block on any of them is a misunderstanding worth
naming. (`type: local` joined the list with meerkat-mob#25. See
[Scheduled refresh for `type: local`](#scheduled-refresh-for-type-local).)

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

```text
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

```text
meerkat_refresh_attempts_total{collection,kind}
meerkat_refresh_changes_total{collection,kind}
meerkat_refresh_failures_total{collection,kind}
meerkat_refresh_skipped_total{collection,kind}          # overlap suppressed
meerkat_refresh_duration_seconds{collection,kind}
meerkat_refresh_last_success_timestamp_seconds{collection,kind}
meerkat_refresh_degraded{collection,kind}               # 0/1
meerkat_collections_ready
meerkat_collections_degraded
meerkat_collection_freshness{state}                     # part C; see below
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
**authenticated collection-discovery surface**, `mk_list_collections`,
as a `refresh` array, one entry per configured target, absent entirely
for a collection that is resolved once. One entry:

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

`mk http serve`'s `GET /collections` carries no such array (#119).
That server runs no refresh cycle, so its entries would describe
configuration alone, as a cycle that never ran.

`mk_list_collections` is the right home for the array: it is already
gated, and already narrowed to the collections the caller may read, so a generation
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

### SIGHUP and `type: local`

A `type: local` collection reads its pages live: `mk_show`, `mk_list` and
the page counts go through its filesystem on every request. Its search
index, however, is built once. Before #105 a long-running
`mk mcp serve-http` therefore could not find a page added after startup,
kept returning a deleted one, and kept an edited page's old snippet,
until someone restarted it. A restart closes the port for as long as
the index takes to build.

SIGHUP now also rebuilds every mounted `type: local` collection:

- **Same funnel.** Each local collection is a refresh target whose
  `Reconcile` is `Collection.ReloadLocal`. Without a `refresh:` block it
  is manual-only: it has no `Spec`, so `Controller.Start` gives it no
  loop, and only `ReloadNow` runs it. With a block it is scheduled too;
  see [Scheduled refresh for `type: local`](#scheduled-refresh-for-type-local).
- **Same discipline.** It takes the reload slot (a second SIGHUP
  mid-rebuild is `ErrBusy`, not a second rebuild) and arms the write
  journal. It re-enumerates the pages through the collection's own
  filesystem, the live directory, and builds the replacement index off
  the request path. It commits with one pointer swap. A failure leaves
  the previous index serving.
- **Without a block, the signal is the change detection.** A path unit
  on a git repository's `HEAD`, a post-merge hook, or an operator decides
  when to send it, and the rebuild always runs. Rebuilding an unchanged
  directory is harmless. The rebuild takes the directory's token first,
  with no span, only so that the new snapshot's version moves (see
  below). With a block, SIGHUP runs the scheduled cycle, probe first.

**Memory: old and new indexes coexist during a swap.** A query in flight
keeps the snapshot it acquired, and the old index is closed only when
its last reader returns (the snapshot's reference count). A memory limit
must therefore leave room for two indexes for a moment. On mk-ai (about
820 pages) the live heap is about 160–200 MB after GC, so a
`GOMEMLIMIT` of 400 MiB and a hard limit of about 768 MB leave that
room.

### Scheduled refresh for `type: local`

SIGHUP needs something outside meerkat to know when the directory moved.
A `refresh:` block on a `type: local` collection moves that decision into
meerkat, through the same controller, the same cycle and the same failure
policies the object stores use (meerkat-mob#25, MK-FRESH-01..03):

```yaml
collections:
  - name: notes
    type: local
    path: /srv/kb/notes
    refresh: {}          # interval 60s, jitter a tenth of it; both may be set
```

The block takes `interval`, `jitter` and `failure_policy` and nothing
else. An unknown key (a typo, or `remote_check:` and `on_divergence:`
before part B defines them) is refused at load. That now holds for every
`refresh:` block, since a block whose interval defaults can no longer
fail on a missing one. An omitted `jitter` defaults to a tenth of the
interval, and an explicit `jitter: 0s` turns the spread off.

**The token** is `kb.FingerprintFS`: a SHA-256 over every candidate
page's `(path, size, mtime)`, in walk order. It is the object stores'
listing fingerprint, with a stat in place of a metadata call. Adding,
deleting, renaming or resizing a page moves it, and so does rewriting one
once the clock has moved on, or a `git pull` or checkout that touches
one. It walks the files `kb.ListFS` considers (one shared `walkPages`),
so a file the index never reads never costs a rebuild. A file `ListFS`
reads and then skips (an OKF navigation artifact, an unparseable page)
costs at most a spare one. It reads no page content, no git metadata,
and runs nothing: pointing meerkat at a knowledge base never runs that
repository's hooks or config (MK-SEC-12). A directory that is not a git
repository refreshes the same way.

**What the token cannot see, and the settle rule.** A modification time
comes from the kernel's coarse clock. It ticks every few milliseconds,
and some filesystems (FAT, several network filesystems) store it to the
second or coarser. Two same-size writes inside one tick leave the token
unchanged. Measured on kernel 6.12, back-to-back same-size writes with a
fingerprint between them left it unchanged 997 times in 1000 on tmpfs.
A probe that lands between two such writes would stamp a stale index
current for good. So the probe also returns the newest page mtime. A
token taken while any page is younger than two seconds (`settleWindow`)
is **unsettled**, and the next probe rebuilds whatever it sees. This is
git's "racy clean" rule. A future mtime counts within the same window; a
far-future one does not, so one badly dated file cannot force a rebuild
every tick. What stays invisible is a same-size rewrite that also keeps
its mtime, such as an mtime-preserving copy. A restart rebuilds
regardless.

**The cycle** is `Collection.ReloadLocal`, the SIGHUP rebuild with a
probe in front of it:

1. Take the reload slot, or `ErrBusy`. A timer tick and a SIGHUP cannot
   both rebuild.
2. Probe: fingerprint the tree. Equal to the serving snapshot's token,
   and that token settled, ends the cycle. That is the common case, and
   it costs a stat per page.
3. Otherwise rebuild off the request path, commit with one pointer swap,
   and stamp the new snapshot with the probe's token.

The token is taken before the pages are read. A change that lands between
the two is in the new index but not its token, so the next probe rebuilds
once more. The opposite order could stamp a token on content the index
never saw, and a stale index would then pass for current. For the same
reason the mount stamps the first snapshot's token before its lazily
built index exists: the index can only be newer than its token.

A failed probe or rebuild leaves the last index serving and marks the
collection degraded under its `failure_policy`, exactly as for an object
store. With the block, SIGHUP runs this same cycle, so an unchanged tree
is not rebuilt. Without the block, SIGHUP still always rebuilds, since the
signal is the change detection there.

Every rebuild now moves the snapshot's version, including a SIGHUP
rebuild of a collection with no block. The link graph keys on that
version (`links.go`), so `linked_from` follows a rebuild. Before, a local
rebuild kept its empty version and served the cached backlinks until a
restart.

**The token stays off spans** (MK-FRESH-10). An object store's version
goes on the cycle span as `meerkat.refresh.version`. A local token is a
fingerprint of somebody's directory, and the design keeps it to
structured status (`ReloadStatus`, `Freshness`) and the log. So
`ReloadLocal` reports it as `Outcome.LogVersion`, which the controller
logs and never puts on a span. A collection without a block emits
exactly the spans it did before: its token is taken with no probe span,
and a failure to take it is not a failure at all (the enumerate phase
reports a broken directory, as it always did).

**A `mount: lazy` child with a block is scheduled from startup.** Its
cycle is a no-op while it is cold, since nothing is loaded. The cache
stamps its token when it mounts it (`cache.go`), and the scheduled cycle
probes from then on. The cycle holds the child's `mountMu`, so a cull
cannot return it to cold underneath a rebuild. A cull resets its record
to `unknown`. A lazy child of any other kind gets no target and no
status slot, so it advertises no schedule that never runs.

**A vanishing subdirectory is skipped, not fatal.** A pull that deletes a
directory while the walk is inside it used to surface as "no content
root" and an empty page list, which a timed rebuild would have swapped
in for one interval. The shared walk now skips a vanished entry and
treats only a missing content root as "no pages".

**`mk mcp serve` runs the controller too**, over the scheduled `type:
local` targets only. stdio has no admin trigger, so a manual-only target
could never run there. With no local `refresh:` block there are no such
targets, `refresh.New` returns nil, and there is no controller, goroutine
or timer (MK-FRESH-09).

**stdio follows `type: local` blocks only** (decided 2026-09-29 by the
operator, #112). An object store's or memory store's `refresh:` block is
followed by `serve-http` alone. Under stdio, a bucket-backed collection
serves the snapshot it resolved at startup until the process restarts,
and a memory store does not pick up other replicas' writes. The
alternative was rejected: every laptop running `mk mcp serve` with an
existing configuration would start polling the bucket with the user's
own credentials, one metadata call per target per interval, for as long
as the MCP client keeps the process alive. That would be new credentialed
traffic on upgrade, opted into only by a block written for `serve-http`.
If someone does run stdio against a bucket, the route is a per-transport
opt-in (`mk mcp serve --refresh`), which is not built.

**The freshness record** (`collections.Freshness`, MK-FRESH-06) is what a
refreshable local collection knows about itself: `loaded` (the token the
index was built from), `on_disk` (what the last probe saw), `remote`,
`behind`, `state` and `checked_at`. The state set is closed, fixed before
all of its producers exist, so later parts add producers and never a
word a client has not seen:

| state | meaning | produced by |
| --- | --- | --- |
| `unknown` | no probe has succeeded yet, or the last one failed; or the local and remote tips differ in a way the check cannot resolve | parts A and B |
| `behind-disk` | the tree moved and the rebuild has not landed; persisting means rebuilds are failing | part A |
| `diverged` | the local branch cannot fast-forward to the remote tip | part B |
| `dirty` | a pull was due and the tree has uncommitted changes; only with `on_divergence: pull` | part B |
| `behind-remote` | the tree is current and the remote tip is ahead of it | part B |
| `current` | the index was built from what the last check saw | part A |

When more than one holds, the first row that applies is the state. A
collection without a `refresh:` block has no record.

meerkat-mob#25 is delivered in three parts, landed in order:

- **A** (this section): the token, the probe, the schedule on both
  transports, and the record, as internal API.
- **B**: git identity, `remote_check:` and `on_divergence:`; see
  [Part B: the working tree and its remote](#part-b-the-working-tree-and-its-remote).
- **C**: the surfaces: `freshness` on `mk_list_collections`, one bounded
  advisory per session per collection in the `mk_search` / `mk_show`
  envelope, `mk collections status [--check]`, and a freshness gauge
  whose only label is the state; see
  [Part C: the surfaces](#part-c-the-surfaces).

### Part B: the working tree and its remote

A `type: local` collection is usually a git working tree, and part B
(meerkat-mob#25) tells it apart from its remote. The design was agreed
on meerkat-mob#25 before code.

```yaml
collections:
  - name: notes
    type: local
    path: /srv/kb/notes
    refresh:
      remote_check: 15m       # off by default; at least 1m
      on_divergence: pull     # flag (default) | pull; pull needs remote_check
```

**Git identity is read, never run.** `internal/gitinfo` finds the working
tree holding the collection's directory (a `.git` directory, or a
`gitdir:` file for a linked worktree, with `commondir`). Like git, it
walks up from the directory, so a collection that is a subdirectory of a
repository, or sits inside one by accident, reports that repository.
With `on_divergence: pull`, that enclosing repository is also what gets
pulled. It reads `HEAD`,
the loose ref or `packed-refs`, and the tracking branch and remote URL
from `config`, with a size cap on every read. The commit goes into the
freshness record as `loaded_commit` and `on_disk_commit`, for display.
Equality is still decided by the fingerprint. The config reader follows
no `[include]` and applies no `url.*.insteadOf`: the URL is the one the
repository spells.

**The remote check** is a separate refresh target (kind `remote`) on its
own `remote_check` cadence. It runs `git ls-remote --heads -- <url>
refs/heads/<branch>`, hardened:

- **Outside the knowledge base.** Its working directory is an empty temp
  directory, repository discovery stops there, and the URL is passed as
  an argument after `--`. So nothing in the KB's own `.git/config`
  applies (no `core.sshCommand`, `credential.helper` or `insteadOf`), and
  a URL cannot be read as an option.
- **`GIT_ALLOW_PROTOCOL=https:ssh:git`.** The URL comes from repository
  config, and `ext::` would make it a command.
- **An environment allowlist.** Only `PATH`, `HOME` and `SSH_AUTH_SOCK`
  pass through, so the user's own ssh agent and global config can still
  authenticate, plus `GIT_CONFIG_NOSYSTEM=1` and `LC_ALL=C`.
- **No prompting** (`GIT_TERMINAL_PROMPT=0`, both askpass variables
  `/bin/false`), and a 30 s timeout.

Its verdicts. R is the remote tip, L the local commit. An object being
**present** locally says nothing about ancestry: any `git fetch` makes R
present while the branch is still behind. So presence is never read as
"contained" (review of #116, M1).

| case | state | note |
| --- | --- | --- |
| R = L | `current` | |
| `flag`, R ≠ L, R **absent** from the local object store | `behind-remote` | |
| `flag`, R ≠ L, R **present** locally | `unknown` | "remote tip fetched but not merged; run the pull or check manually" |
| `flag`, the object lookup hits its cap (64 pack indexes, 250 ms) or alternates | `unknown` | a fixed phrase |
| `pull`, R ≠ L | git decides (below) | |
| no git tree, detached HEAD, no upstream, a refused name, unreachable remote | remote `unknown`, state from the disk probe | a fixed phrase |

The object lookup reads loose objects and each pack's `.idx`: a fanout
lookup, then a binary search, never a whole file. `flag` mode fetches
nothing and walks no history, so it cannot tell behind from diverged,
or a fetched-but-unmerged tip from an unpushed local branch. It says so.
`pull` asks git.

A `behind-remote` verdict clears as soon as the index is built at the
remote tip (a hand pull and a rebuild), without waiting for the next
remote check. A remote or branch name that starts with `-`, or is not a
plain name, is refused before any git call sees it. `git pull` forwards
both to `fetch` without a `--`, where such a name would be an option.
That includes an upstream configured as a URL rather than a remote name
(`branch.<b>.remote = https://…`): it is refused too, and the remote
reads as `unknown`, failing closed.
When `remote.<name>.url` is set more than once, the first value is used,
as git fetches from it.

**A remote check never fails and never degrades** (MK-FRESH-04).
Anything that stops it from answering leaves the remote `unknown`,
explained by a fixed note on the record. The detail, which may name a
URL, goes to the log only (`Outcome.Note`).

**`on_divergence: pull`** acts whenever R ≠ L, the reload slot is free,
and the tree is clean (`git --no-optional-locks status --porcelain
--untracked-files=no` is empty). It runs `git pull --ff-only`, and lets
git decide:

- the branch **fast-forwards to R**: `current`;
- **nothing to fast-forward**, because the branch is ahead: `current`,
  noted "local ahead of remote" (git confirmed it);
- **not a fast-forward**: `diverged`, and the next check says so again;
- the pull lands **somewhere other than R**: `unknown`. That can happen
  when the repository's own config rewrites the URL for the pull.

Then it rebuilds the index under the same reload slot, so the pulled
pages are searchable in the same cycle. Both calls run with `core.hooksPath=/dev/null`
and `core.fsmonitor=false`, under the same environment and protocol
allowlist.

- **A dirty tree** is reported as `dirty`, and the tree is left exactly
  as it was.
- **A branch that cannot fast-forward** is reported as `diverged`, and
  the tree is left exactly as it was.

It never merges, rebases, stashes, checks out or resets. Untracked files
do not count as dirty: a fast-forward refuses to overwrite one anyway.

**The residual risk** of `pull`: it runs inside the repository, so the
repository's own local `.git/config` applies (a checkout filter driver,
for example). That file is written locally and never cloned, and pulling
into a checkout is the operator's opt-in.

### Part C: the surfaces

Part C (meerkat-mob#25) shows the record to the people and agents who
act on it. The design was agreed on meerkat-mob#25 before code, with
three pins noted below. Every surface reads the same record, so none can
disagree with another.

**`mk_list_collections`** (MK-FRESH-06) carries the record as
`freshness` on every collection that has one, and omits the key on every
other collection. It is authenticated discovery, filtered to what the
caller may read, so the commits, the remote tip and the fixed-phrase
note are all there.

**The advisory** is for an agent that never lists collections. When a
collection that `mk_search` searched, or that `mk_show` read from, is
`behind-disk`, `behind-remote`, `dirty` or `diverged`, the result carries
one more line:

```text
freshness: collection "notes" is behind-remote (the remote has commits this server has not loaded)
```

- **It is a second text item, after the result** (pin 1). The first
  item is the JSON it always was, so a client that parses the first
  item keeps working. The advisory is never inside page text, so it
  cannot be mistaken for knowledge-base content. A client that shows
  every item shows two.
- **It is sent once per (session, collection, state)** (pin 2). The
  session is the key `mk_report_outcome` uses: an explicit
  `session_id`, else the MCP session, else one bucket shared by every
  sessionless call. A new state is a new advisory. A key suppresses for
  30 minutes. At most 10,000 keys are kept (expired keys go first, then
  the oldest), and at most five advisories go on one result.
- **It is bounded.** One line, at most 240 bytes, naming the collection
  and the state in a fixed phrase. It never carries a commit, token,
  path or URL. `current` and `unknown` are never advised: `unknown` says
  nothing a caller could act on.
- **It follows the caller's view.** A search advises only on
  collections the caller may read, the same set it searched.

**`mk collections status [--json] [--check]`** (MK-FRESH-08) is the
scripted check. A short-lived process builds its own index as it starts,
so its on-disk half is always current. What it adds is the remote half.
For each collection with `remote_check`, it runs one `ls-remote`, now,
through `ProbeRemote`, which never pulls, whatever `on_divergence`
says. The command reports, and the server acts. With `--check` (pin 3),
it exits 1 when any collection is `behind-remote`, or when a check
could not run because of the checkout's own configuration: not a git
tree, a detached HEAD, no upstream, or a refused name. It exits 0 for
`current` and `unknown`, including an unreachable remote, because a CI
job must not fail when a remote is briefly down. The table says which
problem it was (`config problem:` or `network problem:`).

Two limits follow from reporting without acting (#118 review S2, S3):

- **`dirty` and `diverged` come only from a server's pull.** A
  command that never pulls cannot tell them apart from `behind-remote`,
  so a dirty or diverged checkout that is behind reads `behind-remote`
  and fails `--check` as such. The two states stay in the failing set
  so the rule reads like the server's advisory set.
- **A checkout that fetched without merging reads `unknown` and
  passes.** The flag verdict never walks history (MK-SEC-12), and once
  the tip is present it cannot tell a behind branch from an ahead one.
  CI checkouts usually fetch. There, run
  `git rev-list --count HEAD..@{upstream}` as well: it prints 0 only
  when nothing is left to merge. Most CI checkouts (`actions/checkout`
  included) are a detached HEAD, where `--check` exits 1 with `HEAD is
  detached` and `@{upstream}` does not resolve. Those compare with
  `git rev-list --count HEAD..origin/<branch>` instead.

**`mk version`** names the commit each refreshed local collection is
checked out at, read from git's files. It stays offline: no network, no
git.

**`meerkat_collection_freshness{state}`** (pin 4) counts collections in
each state. It is computed at scrape time from the records, so it cannot
drift from `mk_list_collections`. Its only label is the state, a closed
set of six: no ordinal, because the gauge counts rather than
identifies, and no name, path, commit or token (MK-FRESH-10). Only
collections with a record are counted. With none, the collector emits
nothing, and an unconfigured server's `/metrics` is what it was.

**Not on `mk http serve`'s `GET /collections`.** That server loads its
collections once and never reconciles: it runs no controller and no
remote check. A freshness record there would be frozen at the moment of
mount, and would read `current` for as long as the process lives. No
answer is better than a wrong one. It gains the field when it gains a
controller. Its help, the README and its OpenAPI description say so.
The same endpoint's older `refresh` array had the same problem, and
was removed for the same reason (#119).

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
- **Local refresh (`internal/kb`, `internal/collections`,
  `internal/mcp`):** the fingerprint moves on add, delete, rename, resize
  and mtime, and not for non-page files; a vanishing subdirectory no
  longer empties the page list; an unchanged, settled tree rebuilds
  nothing; a same-size rewrite inside one timestamp tick is still indexed
  (the settle rule); add, edit and delete each agree across search, show
  and the page list after one cycle; a failed probe serves the last good
  index and degrades, and recovers; the scheduled path finds a new page
  with nobody calling anything; a lazy child is a no-op while cold and is
  stamped and probed once mounted; a rebuild moves backlinks; the manual
  target still always rebuilds; no span carries a local token or path,
  and a collection without a block emits no probe span; stdio has no
  controller without a local block and never polls an object or memory
  store; unknown `refresh:` keys are refused; the state set is pinned.
- **Hosted surface:** refresh is off unless configured and publishes no
  metrics then; a failed cycle produces `status: degraded` with HTTP 200,
  the documented counts, and the documented series — with no collection
  name, bucket or prefix anywhere in either; `unready` produces a 503;
  and the freshness detail those two omit does reach authenticated
  collection discovery, with `last_success` left untouched by the
  failure.
- **Part C surfaces (`internal/collections`, `internal/mcp`,
  `internal/cli`, against bare remotes on disk):** which states are
  stale and advised; the config/network split of a failed remote check;
  the advisory stays one valid UTF-8 line within 240 bytes whatever the
  name; `ProbeRemote` never pulls under `on_divergence: pull`, with
  `CheckRemote` on the same fixture as the control. On the MCP surface:
  `freshness` is present where configured and absent (not `null`)
  elsewhere; the advisory is the second item, the first still parses;
  it is sent once per session, again for a new `session_id`, again for
  a new state (behind-remote, then dirty) and after the TTL; the
  tracker stays within its cap; the gauge is absent without records and
  labelled by state only with them, on the hosted `/metrics` too. On
  the CLI: `--check` fails on behind-remote and a config problem, and
  passes on current, an unreachable remote and a fetched-but-unmerged
  checkout (`unknown`); `--json`; the command never pulls; `mk version`
  reports the commit with the remote gone. The advisory tracker: at most
  five per result, oldest-first eviction at the cap, expired keys
  leaving before a live one, and a constant cost per call at the cap
  (`BenchmarkAdvisory_TakeAtCap`). The disclosure test
  (`TestObservability_FreshnessCarriesNoNamePathCommitOrToken`) runs a
  content cycle, a remote check and the pull it triggers, with tracing
  on and the gauge registered. It asserts that no span or label carries
  a collection name, a directory, the remote's path, either commit or a
  token. Each protection's test was checked to fail with the protection
  removed.

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
- **Other object stores.** Done for S3: the probe is `ObjectVersion`
  (ETag for a bundle, a `(key, ETag, size)` listing fingerprint for a
  prefix) and `S3Store` implements `Fingerprinter`. See
  `docs/design/object-stores.md` for what each provider enforces.
- **Scheduled, change-detecting refresh for `type: local`.** Part A is
  done: see
  [Scheduled refresh for `type: local`](#scheduled-refresh-for-type-local).
  Part B (git identity, remote check, divergence ladder) and part C
  (surfaces) are done too. See
  [Part B](#part-b-the-working-tree-and-its-remote) and
  [Part C](#part-c-the-surfaces).
- **systemd socket activation (`LISTEN_FDS`).** Done (#110):
  `serve-http` serves on a socket systemd passes in, so systemd holds the
  port across a restart. See the README's "Socket activation (systemd)".
