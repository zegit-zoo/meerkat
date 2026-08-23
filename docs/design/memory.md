# Spec: Memory toolset — personal, team and global memory with review

**Status:** Implemented (`internal/memory`, `mk_save_memory` in `internal/mcp`; `memory:` in content-source.yaml) · **Builds on:** [hosted-mcp.md](hosted-mcp.md) · **Issue:** #10

*(`docs/design/` is a design record. The up-to-date schema reference is
[content-source.example.yaml](../../content-source.example.yaml); the
user-facing reference is the [README](../../README.md).)*

## Summary

Every surface meerkat had was a read. An agent could search a knowledge
base and could not add to one, so anything learned during a session —
a decision and its reasoning, a convention, a correction the user made —
was gone at the end of it.

This spec adds one MCP tool, **`mk_save_memory`**, which writes a
structured Markdown document into a collection's **memory store** at one
of three scopes, and makes it searchable in the same breath. It is the
first thing in meerkat that writes on behalf of a remote caller, so
almost all of the design below is about the two questions that creates:
*whose* memory is this, and *what happens when two callers write the
same one*.

It also consumes the capability model #9 defined and left unenforced.

## Non-goals

- **A GitOps / merge-request review pipeline.** An unauthorized team or
  global write becomes a **staging artifact** in a clearly-namespaced
  directory and the response says where it landed. Promoting one is an
  operator action (move the file, or run whatever the deployment already
  uses). Wiring meerkat to a forge to open a PR per memory is a separate
  piece of work — see [Future work](#future-work).
- **Deleting or editing memories through MCP.** `mk_save_memory` creates
  and updates. There is no `mk_delete_memory`: removal is an operator
  action against the store, where it is auditable.
- **Per-page read authorization.** Unchanged from #9: **the collection
  is the unit of read access.** "Personal" describes who may WRITE a
  memory, not a private read channel — a personal memory is readable by
  everyone who can read its collection. This is stated loudly because
  the word invites the other reading, and a test pins it
  (`TestHostedMemory_APersonalMemoryIsVisibleToTheCollectionsReaders`).
- **Cross-process locking for the local backend.** Two meerkat processes
  writing one local directory cannot be serialised portably; that
  deployment should use the GCS backend, whose preconditions the backend
  itself enforces.
- **Memory in the HTTP/OpenWebUI server.** `mk http serve` is unchanged,
  static token and all.

## Identity, and why spoofing is structural rather than checked

The requirement is that a caller cannot write into somebody else's
personal namespace. The weak way to satisfy it is a check: accept a
`namespace` argument, compare it to the token, refuse a mismatch. That
is one forgotten call site away from being wrong, and the failure is
silent.

So there is **no argument that names a principal.** Not `namespace`,
not `subject`, not `owner`, not `author`. The namespace comes from one
function with one input:

```go
memory.Namespace(grants.Identity())   // authz.Identity, from the verified token
```

and `Authorize` returns the identity rather than accepting one, so no
call site can supply a different one:

```go
func Authorize(g *authz.Grants, collection string, scope Scope, allowAnonymousPersonal bool) (Decision, error)
//                              ^ no identity parameter; Decision carries the verified one out
```

A caller controls the `key`, the `title`, the `content`, the `tags`, the
`scope` and the `collection`. Those choose **where inside their own
space** a memory goes. Nothing chooses **whose space that is.**

### The namespace itself

```
sha256(issuer + "\x00" + subject)[:16]   , prefixed with a readable slug of the subject
```

- **`(issuer, subject)`, and nothing else.** Email, groups and tenant
  are *not* inputs. A person who changes team, address or tenant must
  keep their memories; a namespace that moves when a directory attribute
  changes is a namespace that silently orphans data.
- **The readable prefix is cosmetic.** The hash carries the uniqueness,
  so two subjects that sanitize to the same slug still get different
  namespaces. It exists so an operator looking at the directory can tell
  whose memories they are looking at.
- **Two issuers are two principals.** Same `sub` from a different `iss`
  is a different namespace, matching how the policy layer already treats
  them.

### Path safety

`Slug` maps anything that is not `[a-z0-9_]` to `-`, collapses runs,
trims and truncates. There is no input for which it returns something
containing `/`, `\` or `..`, so a crafted key resolves to a leaf name
inside the namespace the *identity* chose. Three layers back that up,
in the order they run:

1. `Slug` — the key can only ever be one path component;
2. `checkKey` — a re-assertion at the backend boundary, because a
   backend is a public type and the next caller may not have read
   `Slug`'s doc comment;
3. `os.Root` — the local backend writes through a rooted handle that
   refuses to traverse out of the store, including through a symlink
   stored inside it.

### Anonymous callers

Two deployments have no verified subject: **stdio** (spawned by the one
user it serves, over a pipe) and **`allow_unauthenticated`** (a gateway
authenticated, meerkat did not). They are not the same case:

| transport | anonymous personal write |
| --- | --- |
| stdio | **allowed**, in the fixed `local` namespace — one user, no ambiguity |
| hosted | **refused** — every anonymous caller would otherwise share one namespace |

That is one boolean, `memoryOptions.AllowAnonymousPersonal`, set at
server construction and true in exactly one place. Team and global
writes need no identity and are unaffected.

## Scopes and capabilities

| scope | capability | held | not held |
| --- | --- | --- | --- |
| `personal` | `personal-write` | write | **refuse** |
| `team` | `team-write` | write | **stage**, if any write capability is held |
| `global` | `global-write` | write | **stage**, if any write capability is held |

`admin` implies all three, as it implies everything.

**Personal has no staging row.** A personal memory has no reviewer — it
is nobody's business but the caller's — so an unauthorized personal
write is refused rather than parked somewhere a stranger has to triage.

**Staging requires being a writer somewhere in the collection.** A
caller who holds `personal-write` and asks for `team` is proposing; a
caller who holds only `read` is not offered the tool at all (below). So
the staging area cannot be filled by anyone who merely reached the
process.

### Why `global-write` is a new capability

#9 defined `read`, `personal-write`, `team-write` and `admin`. "Global"
had no capability, and the two available shortcuts were both wrong:

- **Overload `admin`.** `admin` explicitly implies *every capability,
  present and future*. Folding global writes into it would retroactively
  widen every `admin` rule an operator has already written — a rule
  granted in the #9 world would silently start granting the power to
  publish to everyone.
- **Overload `team-write`.** Same shape, one level down: every existing
  team rule would gain a broader power than its author granted.

So `global-write` is its own capability, in `AllCapabilities` between
`team-write` and `admin`. Adding a value to that list is additive:
`ParseCapability` starts accepting it, `CapabilitySet.Has` keeps
answering `true` for it under `admin` (which is correct — that IS what
admin means), and no existing rule changes meaning. A test pins that
neither `read`, `personal-write` nor `team-write` confers it.

## Invisibility on the write path

#9's load-bearing rule is that an unauthorized collection is *invisible,
not denied*, and that it is enforced by narrowing the registry once
rather than by a check per operation. The write path inherits that
mechanism rather than reimplementing it:

```
request → verify token → evaluate policy → Registry.Restrict(grants.CanRead)  → read handlers
                                        └→ Registry.Restrict(grants.CanWrite)
                                               .WithMemory()                  → mk_save_memory
```

Two views, deliberately not one:

- `Restrict(CanRead)` is read-shaped in **both** directions. A rule
  granting `personal-write` without `read` is unusual but coherent
  ("drop notes here, don't read the others'"), and a read-shaped view
  would hide from its holder the very collection they were granted.
  Conversely a rule granting only `read` makes a collection visible, and
  its holder must *not* be offered a tool they could only ever be
  refused by.
- `Restrict(CanWrite)` is **strictly narrower** than the read view, so
  it can leak nothing the read path does not already. Everything that
  falls out of a restricted registry falls out here too: the
  unknown-collection error names only collections in the caller's own
  view, so naming a collection they hold nothing over produces the
  identical sentence to naming one nobody mounted.

`.WithMemory()` then drops collections with no `memory:` store, so the
tool's description enumerates only collections that can actually accept
a memory.

`WithToolFilter` runs on `tools/call` as well as `tools/list`, so this
is an access boundary and not a display fix. A caller with no write
capability anywhere is not offered `mk_save_memory` and **cannot invoke
it** — they are refused by the transport, one step earlier than the
handler.

| caller holds | KB tools | `mk_save_memory` |
| --- | --- | --- |
| `read` on X | yes, over X | no |
| `personal-write` on X | none | yes, over X |
| `read` + `team-write` on X | yes, over X | yes, over X |
| nothing (no rule matched) | — | — (403 at the gate) |

The gate's 403 is unchanged and did not need to be: `Grants.Empty` was
already capability-agnostic, so a write-only principal passes it. Its
doc comment now says so explicitly, since that is now load-bearing.

## Storage

A memory store is **a directory, or a GCS prefix, that is NOT part of
the collection's served content tree.**

```
<store>/personal/<namespace>/<slug>.md
<store>/team/<slug>.md
<store>/global/<slug>.md
<store>/_staging/<scope>/<namespace>/<slug>.md      ← pending review
```

The "not part of the content tree" bit is the load-bearing half. If the
store lived under `wiki/`, the staging area would live there too, and an
unauthorized caller's pending artifact would be picked up by
`kb.ListFS` and indexed **on the next restart** — turning "staged for
review" into "published, one restart later". Keeping the whole store
outside the served tree means `_staging/` is excluded *by construction*:
`Store.Load` skips it, and nothing else ever reads the store.

Documents reach the collection through a **page overlay** instead (see
[Immediate searchability](#immediate-searchability)), which also means
the mechanism is identical for a GCS-backed store, where there is no
local tree to write into at all.

### Configuration

```yaml
collections:
  - name: team-notes
    type: local
    path: ../notes
    memory:
      type: local
      path: memory          # relative to the collection's content dir; default "memory"

  - name: shared
    type: gcs
    bucket: my-org-knowledge
    prefix: kb/live/
    memory:
      type: gcs
      bucket: my-org-knowledge
      prefix: kb/memory/
```

No `memory:` block means the collection is **read-only** and
`mk_save_memory` will not name it — which is what every configuration
that predates this change has, and why a deployment that configured
nothing sees no new tool at all.

Validation runs at **load** time, as `auth:`'s does. The interesting
rule: a *relative* local `path:` under a `type: url` or `type: gcs`
content source is **refused**, because such a source resolves to a
content-addressed cache directory that is replaced whenever the content
changes — the memories would appear to vanish on the next content bump.
The error says to use an absolute path or the GCS backend. This is data
loss caught at startup rather than discovered later.

### Optimistic locking

Every write carries a precondition. There is deliberately **no
unconditional overwrite**: the zero `Precondition` is refused by both
backends, because an unconditional write is exactly how two agents
saving to the same key silently lose one of the two memories.

```
version given      →  update exactly that revision
replace: true      →  read the current revision, then update from it
neither            →  create-only
```

| | local | GCS |
| --- | --- | --- |
| version token | `sha256(bytes)[:16]` | object generation |
| create | `O_EXCL`-style check under the store lock | `ifGenerationMatch: 0` |
| update | compare hash under the store lock | `ifGenerationMatch: <n>` |
| enforced by | this process | **the backend** |
| durability | temp file + `fsync` + atomic rename | GCS |

A failed precondition is a `*ConflictError` wrapping `ErrConflict`,
carrying the revision that is actually there, and the tool renders it as
the exact next call to make: *"It is now at version X. Read it with
mk_show, merge what you wanted to add, and save again with
version=X."* It is a **retryable** condition, not a permission failure,
and it is never retried automatically — an automatic retry would be
last-writer-wins wearing a CAS costume.

Two details worth stating:

- **`replace: true` is still a compare-and-swap.** It reads the version
  and conditions the write on it, so a save that interleaves between the
  two fails rather than clobbering someone's work. It is a convenience
  for the caller, not a relaxation of the guarantee.
- **Updating something that isn't there is a conflict, not a create.**
  Silently creating it would resurrect a memory somebody deliberately
  removed.
- **A byte-identical rewrite leaves the local version unchanged.**
  Content-addressing means "nothing changed" invalidates nobody's
  precondition. (Tags are sorted and de-duplicated on render partly so
  that this holds for a re-save with the same tags in a different
  order.)

**Local is single-process.** The lock is a mutex, so two meerkat
processes sharing one directory are outside what the backend can
serialise — POSIX offers no portable compare-and-swap on a file. That is
documented on `LocalStore` and is the reason the GCS backend exists: its
preconditions are evaluated server-side, so several replicas can share
one store and still never lose a write.

## Immediate searchability

The acceptance criterion is that a saved memory is searchable *by the
next call*, with no restart. Three steps, in this order:

1. **Store**, under its precondition. Nothing becomes searchable that
   was not durably stored.
2. **Overlay.** The page goes into `Collection.overlay`, a map guarded
   by an `RWMutex` that `Pages()` merges and `Load()` consults first.
3. **Index.** `search.Index.Put(page)` calls bleve's incremental
   `Index(id, doc)` on the **live** index — no rebuild. Re-indexing an
   existing ID replaces that document, so a re-save updates rather than
   duplicating.

Order matters: the durable write is first, so a conflict costs nothing,
and the index update is last, so a refused write cannot reach it.

At mount time `AttachMemory` loads the store's live documents into the
overlay, so memories written by an earlier process are searchable from
this one's first request. A document that fails to parse is skipped with
a warning rather than failing the mount — one malformed memory must not
make a whole collection unserveable.

**Concurrency.** The registry shares one `*Collection` (and therefore
one overlay, one store and one index) across every MCP session, so all
three are written while other sessions read. The overlay is under
`RWMutex`; bleve serialises its own writes against readers; `Index`'s
page map, which was previously written once at construction and only
read, gained an `RWMutex` of its own.

**Page IDs** live under a reserved `memory/` prefix
(`memory/personal/<ns>/<slug>`), so `mk_list --prefix memory/` lists
exactly the memories, and a memory is distinguishable from an ingested
page at a glance. A document's own `id:` frontmatter is ignored in
favour of its store key (`kb.ParsePage` takes the ID from the caller),
because trusting the body would let whoever can write one memory shadow
any page in the collection.

## Document format

```markdown
---
id: memory/personal/alice-8f2a1c0b9d8e7f60/deploy-checklist
title: Deploy checklist
type: Memory
category: memory
status: saved                # or pending-review, for a staged artifact
tags: [deploy, ops]
generated:
  by: meerkat/mk_save_memory
  at: 2026-08-23T10:00:00Z
memory_scope: personal
memory_namespace: alice-8f2a1c0b9d8e7f60
memory_key: deploy-checklist
memory_subject: <sub>
memory_issuer: https://idp.example.com
---

Always drain the node first.
```

`type: Memory` and `category: memory` make memories filterable through
the existing `mk_list` facets with no new argument. `generated:` is
OKF's provenance family (SPEC.md §5.2), so a memory carries the same
trust signals as any other page and reads as `unverified` until somebody
verifies it — which is correct.

The `memory_*` keys are written as **top-level** frontmatter, not under
`extra:`. `kb`'s parser collects unknown top-level keys *into* `Extra`,
so top-level is what round-trips; `kb.MarshalFrontmatter` would have
nested them one level deeper than they parse back out. Hence a small
local frontmatter struct rather than a reuse of `kb.Frontmatter`.

## Staging shape

Minimal, on purpose.

```
<store>/_staging/<scope>/<namespace>/<slug>.md      status: pending-review
```

- Written unconditionally — a proposal supersedes an earlier proposal
  for the same key rather than colliding with it.
- Carries the proposer's namespace in the path even for `team`/`global`,
  so a reviewer sees who proposed what without opening the file.
- **Never** loaded, indexed, listed or shown. `Store.Load` skips it in
  both backends — matching *any* path segment named `_staging`, not just
  a leading one, because publishing an unreviewed proposal is the
  failure that matters and the permissive reading is the one that causes
  it. The store is outside the content tree, so nothing else can reach
  it either.
- The response says `status: "staged"`, `searchable: false`, the exact
  location, and which capability would promote it — so the model can
  tell the user where it went instead of reporting a success that
  wasn't one.

Promotion today is `mv <store>/_staging/<scope>/<ns>/<slug>.md
<store>/<scope>/<slug>.md`, plus flipping the document's
`status: pending-review` to `saved`. There is no meerkat command for it
yet, and the document's `id:`/`memory_scope:` are already written as the
promoted values, so nothing else needs editing.

## Tool surface

```
mk_save_memory(scope, title, content, [key], [tags], [version], [replace], [collection])
  → {status, collection, scope, namespace, id, version, location, searchable, note}
```

Annotations are the read tools' inverted where they should be:
`readOnlyHint: false` (it writes), `destructiveHint: false` (it adds or
updates one document and removes nothing), `idempotentHint: false` (a
second call with no key writes a second document; with a stale version
it is a conflict — that is the point), `openWorldHint: true` (a
GCS-backed store is a real external system).

An omitted `collection` with several in view is an **error naming
them**, not a pick. Guessing which knowledge base a memory belongs in is
the one mistake here the caller could not undo, because they would not
know it had happened.

## Back-compat

| deployment | after this change |
| --- | --- |
| no `memory:` block anywhere | no `mk_save_memory` tool is registered at all; every other surface byte-identical |
| `mk mcp serve` (stdio) | unchanged, plus the memory tool if a store is configured; personal memories land in the `local` namespace |
| `mk http serve` | unchanged — no memory surface |
| an existing `auth:` policy | unchanged meaning; `global-write` is new, so no existing rule gains a power |
| `--kb-dir` | still suppresses `content-source.yaml` discovery, and therefore `memory:` too |

`collections.Open` gained a `context.Context` (memory stores do network
work at mount time, and a store that cannot be reached should fail the
process at startup where a health gate catches it — the same call this
package already makes about indexes).

## Testing strategy

- **Namespace derivation** (`internal/memory/memory_test.go`): stable
  under email/group/tenant change, distinct per issuer and per subject,
  distinct for two subjects that slugify identically, and `Slug` against
  a battery of traversal payloads asserting no output can contain `/`,
  `\` or `..`.
- **Spoofing, end to end** (`internal/mcp/memory_test.go`): a real
  attacker session against a real victim session, trying crafted keys
  (`../<victim-ns>/salary`, percent-encoded variants, absolute paths)
  *and* inventing `namespace`/`subject`/`owner`/`author` arguments that
  do not exist — then asserting the victim's document is byte-for-byte
  unchanged.
- **Capability matrix**: exhaustive scope × capability, in
  `internal/memory/authorize_test.go` as a unit and again over the wire.
  Includes the assertion that a reader is refused by the *transport*
  (the tool is not offered) rather than by the handler.
- **Invisibility on the write path**: a hidden collection's error is
  compared character-for-character against an absent collection's, the
  tool's JSON schema is asserted not to contain the hidden name, and the
  hidden store is asserted to be empty afterwards.
- **Optimistic locking**: 8–16 concurrent writers to one key, in each
  backend and over the wire, asserting **exactly one** winner and that
  every loser is *told* it lost.
- **Immediate searchability**: index built *before* the write, so the
  test proves an incremental update rather than a lucky first build;
  then asserted through `mk_search`, `mk_show`, `mk_list` and from a
  second session.
- **Staging**: asserted absent from search, show and list, present on
  disk under `_staging/`, and still absent after a remount.
- **GCS** (`internal/memory/gcs_test.go`): a fake implementing real
  generation semantics. No credentials, no bucket, no network — the same
  seam `internal/contentsource` uses for the read side.

## Future work

- **A real review pipeline.** Staging is a directory. Opening a merge
  request per proposal (reusing `internal/auth`'s forge credentials),
  or an `mk memory review` / `mk memory promote` command, is the obvious
  next step and explicitly out of scope here.
- **Deletion and expiry.** No `mk_delete_memory`, and no TTL. OKF's
  `stale_after` is available for a caller to set by hand today; a
  scope-level default would be a small addition.
- **Cross-process locking for the local backend.** An advisory lock file
  would cover the two-replicas-one-volume case that currently has to use
  GCS.
- **A memory-aware read tool.** Memories are found by `mk_search` and
  `mk_list --prefix memory/` today. A dedicated `mk_recall` that filtered
  to the caller's own namespace by default would be a better default
  than a prefix filter the model has to remember.
- **Per-page read authorization**, which would let "personal" mean
  private-to-read as well as private-to-write. That is a much larger
  change to the authorization model — the collection is the unit today
  — and is not implied by anything here.
