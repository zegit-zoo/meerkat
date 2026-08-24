# Spec: Memory toolset — personal, team and global memory with review

**Status:** Implemented (`internal/memory`, `mk_save_memory` in `internal/mcp`; `memory:` in content-source.yaml) · **Builds on:** [hosted-mcp.md](hosted-mcp.md) · **Issues:** #10, #27 (private personal reads)

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

**#27 added the third question:** *who may read it.* The original answer
— the collection is the unit of read access, and `personal` describes
who may write — was the wrong one for a hosted deployment, where
"remember this for me" reasonably means private. A personal memory is
now readable by the principal whose namespace it is in and by nobody
else, and unreadable is indistinguishable from nonexistent. See
[Private personal reads](#private-personal-reads-27).

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
- **General per-page read authorization.** #27 made *personal memories*
  private to read as well as to write (see [Private personal
  reads](#private-personal-reads-27)), but it did not build a row-level
  authorization language for arbitrary KB pages. The collection remains
  the unit of read access for ordinary content; the one exception is a
  page whose ID says whose it is.
- **Team-private reads.** `team` still means "every reader of the
  collection". Deriving a team identity from claims and scoping reads to
  it is a separate decision with a separate blast radius — see
  [Future work](#future-work).
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

Three deployments have no verified subject: **stdio** (spawned by the one
user it serves, over a pipe), **`allow_unauthenticated`** (a gateway
authenticated, meerkat did not), and — since #36 — a caller admitted by
an **`anonymous: true` policy rule** (the hosted server published a
collection to callers with no token). The last two are the same case as
each other, and neither is the first:

| transport | anonymous personal write | anonymous personal read |
| --- | --- | --- |
| stdio | **allowed**, in the fixed `local` namespace — one user, no ambiguity | the `local` namespace, which is the one it writes into |
| hosted | **refused** — every anonymous caller would otherwise share one namespace | **nothing** — a server that cannot name its caller must not hand out somebody's private memory either |

That is one boolean, `transportOptions.AllowAnonymousPersonal`, set at
server construction and true in exactly one place. It now answers the
same question on both sides of the feature — *when nobody
authenticated, who is this?* — which is why the type is no longer named
for the write path alone. Team and global writes need no identity and
are unaffected.

## Private personal reads (#27)

The original design said the namespace decided who may **write** a
personal memory, and that the collection remained the unit of **read**
access. That was a defensible line for #10 and the wrong one for a
hosted, multi-user deployment: "remember this for me" means *for me*,
and a note a caller kept for themselves — a correction, a preference, a
summary of a conversation — was readable by every other reader of the
collection.

#27 closes that. **A personal memory is readable by the principal whose
namespace it is in, and by nobody else.** Not merely refused to
everybody else: *absent*. It does not appear in their search results,
their listing, their page counts or their snippets; asking for its ID
answers exactly as asking for an ID nobody ever wrote; and it is not
counted by the ambiguity error. That is the same invisibility property
#9 established for collections ([hosted-mcp.md](hosted-mcp.md)), applied
one level down, and it is held to the same standard by the same kind of
test.

### The owner is the page ID

The first question is where "whose is this?" lives. Three candidates,
and only one of them cannot be forgotten or forged:

| carrier | failure mode |
| --- | --- |
| a frontmatter field | the document's own bytes claim it. A caller writes the body; trusting it would let whoever can write one memory claim another's — the same reason `kb.ParsePage` already ignores a document's `id:`. |
| a struct field stamped by a constructor | a constructor that forgets it produces a **public** page. The failure is silent and in the wrong direction. |
| **the page ID** | none available: the ID is built by `memory.Resolve` out of the identity-derived namespace and nothing a caller supplied. |

So the owner is derived, at every enforcement point, from the ID:

```
memory/personal/<namespace>/<slug>   private to <namespace>
anything else                        visible to every reader of the collection
```

`kb.PrivateOwner(id)` is that derivation, and `kb.Viewer` is the value
that answers "may this viewer see it". Both live in `internal/kb`
because that package defines what a page ID *is*; a test in
`internal/memory` pins `kb.PrivateOwner(ref.PageID) == ref.Namespace`
for every scope, so the two packages' notions of the layout cannot drift
apart quietly.

Two consequences worth stating:

- **`memory/personal/` is a reserved page-ID prefix everywhere.** An
  ingested content page that happens to sit at
  `content/memory/personal/x/y.md` is treated as private to `x` too.
  That is the safe direction (it becomes harder to read, not easier), it
  removes any question of whether the content tree or the overlay is
  authoritative about an ID's owner, and it means a private overlay
  document cannot make a public content page vanish for everybody else
  by sitting on its ID — the overlay only shadows a content page for the
  viewers who can actually see the overlay entry.
- **The namespace is already a sufficient owner key.** It is
  `sha256(issuer + "\x00" + subject)`, so two issuers minting the same
  `sub` are two owners, and an email or group change moves nothing. The
  read side inherits all of that for free rather than re-deriving it.

### Filtering happens in the query, not over the results

The load-bearing implementation decision. Visibility is a **mandatory
clause in the bleve query**, conjoined with the existing title/id/body
disjunction:

```
Conjunction(
    Disjunction( owner:public , owner:own:<caller> ),   ← boost 0
    Disjunction( title^5 , id^3 , body , category boosts... ),
)
```

Post-filtering the top *N* results would be both a leak and a bug:

- **A bug**, because bleve ranks and truncates to `limit` *inside* the
  search. A caller whose ten best-scoring documents are somebody else's
  private memories would get an empty result and be told, truthfully
  from its point of view, that the knowledge base has no answer. A test
  pins exactly this: fifty private documents that all outrank the one
  public match, a limit of five, and the assertion that the public match
  comes back.
- **A leak**, because a post-filter has to have loaded and scored the
  metadata it then throws away — which is how counts, snippets and
  timing differences escape.

The clause is boosted to **zero** so it decides eligibility without
touching ranking: a conjunction's score is the sum of its children's,
and a zero query boost zeroes both the term's weight and its score
contribution. A test asserts score-for-score parity between a restricted
and an unrestricted query over the documents both can see, so
"visibility does not change relevance" is pinned rather than assumed.

An **unfiltered** viewer adds no clause at all, so the query the CLI
runs is byte-for-byte the one that ran before any of this existed.

The `owner` field is indexed with the `keyword` analyzer (one verbatim
token — the standard analyzer would split `own:alice-8f2a…` on the
colon and the dash and let one owner's token match another's) and with
`IncludeInAll: false`, so it is not reachable from a free-text query: a
caller cannot search for another principal's token and learn from the
hit count that the principal exists.

`indexDoc` is shared by the bulk build and by `Index.Put`, so a memory
indexed incrementally into a live index carries the same visibility
field as one present at startup. There is no per-user index and no
rebuild: one shared index, one extra field, one extra clause.

### One seam, two narrowings

The MCP surface applies authorization in exactly one place, and #27 did
not add a second:

```go
func visible(ctx, reg, mem) *collections.Registry {
    view := reg
    if g := authz.FromContext(ctx); g != nil {
        view = reg.Restrict(g.CanRead)   // which COLLECTIONS exist
    }
    return view.ViewedBy(mem.viewer(g))  // which PAGES exist inside them
}
```

`Registry.ViewedBy` is the per-page counterpart of `Restrict` and has
deliberately the same shape: it returns a derived, borrowing view
carrying the viewer, and every read below it — `Pages`, `Search`,
`Show`, and therefore the ambiguity error and the page counts — reads
that viewer without knowing it exists. There is no per-operation viewer
argument for a call site to forget, and a read method added later
inherits the policy for free, exactly as `Restrict` covers methods added
after it.

The two compose in either order and a test pins that they do.

`show` and `list` are enforced by the same value rather than by a
parallel check. `Collection.LoadFor` checks the requested **ID** before
it loads anything (so an unauthorized lookup does no work and touches no
store) and the returned **page** afterwards (because the ID a caller
passed and the page that came back are two different things), and
answers `kb.ErrNotFound` either way. `Registry.Show` calls `LoadFor`, so
an invisible page is not *withheld* from the ambiguity count — it is
never found, and the count is right by construction.

### Who the viewer is

The viewer's owner comes from the verified identity and from nothing
else. `transportOptions.viewer` is the whole derivation:

| caller | viewer |
| --- | --- |
| verified OIDC identity | `AsOwner(memory.Namespace(id))` — their own personal memories, plus everything public |
| anonymous, **stdio** | `AsOwner("local")` — the namespace this user's own memories are written into |
| anonymous, **hosted** (no `auth:` block, `allow_unauthenticated`, or an `anonymous: true` rule) | `AsOwner("")` — every public page and **no** personal memory belonging to anyone |
| no MCP transport at all (`mk search/list/show`, `mk http serve`) | `Unfiltered()` — one principal in front of it, who owns everything |

The third row is the one worth arguing. A hosted server with no
providers cannot tell its callers apart, so it cannot tell whose a
personal memory is — and it already refuses to let such a caller
**write** one for exactly that reason. Handing every anonymous caller
the `local` namespace would give them, as a group, read access to
whatever a stdio session wrote into a shared store. Owning nothing is
the only answer that stays true when meerkat does not know who is
asking.

The fourth row is why the registry's viewer is a **pointer**: "nobody
attached a viewer" (unrestricted — the CLI, the static-token HTTP
server, every pre-existing caller) has to stay distinguishable from
`AsOwner("")` (a caller who owns nothing). Collapsing them into one zero
value would either break single-user surfaces or unhide private pages
from anonymous ones, depending on which way it fell.

### Configuration

```yaml
memory:
  type: gcs
  bucket: my-org-knowledge
  prefix: kb/memory/
  personal_visibility: private     # private (default) | collection
```

- **`private` is the default**, including for every configuration
  written before the key existed and for a collection with no `memory:`
  block at all. A word that means privacy must not need an opt-in to
  provide it.
- **`collection`** is the pre-#27 behaviour, named for what it does
  rather than for the version it dates from: personal memories are
  readable by every reader of the collection. Configuring it alongside
  `auth.providers` produces a startup **warning** naming the collection
  — under authentication meerkat knows exactly whose each memory is and
  has been told to show it to everyone anyway, and that decision belongs
  in the log of the process acting on it. Without OIDC there is no
  principal to be private from, so the setting changes nothing there and
  is not worth a line.
- An unrecognised value is a **load-time error**, not an ignored line —
  the same rule `ParseCapability` follows, for the same reason: "the
  deployment ignored the word I wrote" is how a store ends up more
  readable than its operator believes.

The policy is per-collection because the `memory:` block is, and it is
applied in `Collection.viewerFor`: a collection that opted out widens
every viewer to an unfiltered one. The page metadata stays
policy-free — one place decides, and it is not the document.

## Scopes and capabilities

| scope | capability | held | not held | who may READ it |
| --- | --- | --- | --- | --- |
| `personal` | `personal-write` | write | **refuse** | the owning principal alone (see [Private personal reads](#private-personal-reads-27)) |
| `team` | `team-write` | write | **stage**, if any write capability is held | every reader of the collection |
| `global` | `global-write` | write | **stage**, if any write capability is held | every reader of the collection |

`admin` implies all three write capabilities, as it implies everything.
It does **not** confer read access to another principal's personal
memories: `admin` is a capability over a *collection*, and ownership is
not a capability. An operator who needs to reach a personal memory does
it at the store — where it is auditable — or configures
`personal_visibility: collection` for that collection and accepts what
that says.

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
      personal_visibility: private   # private (default) | collection
```

No `memory:` block means the collection is **read-only** and
`mk_save_memory` will not name it — which is what every configuration
that predates this change has, and why a deployment that configured
nothing sees no new tool at all.

`personal_visibility:` is the read policy, described under [Private
personal reads](#private-personal-reads-27). It defaults to `private`,
so a `memory:` block written before the key existed keeps personal
memories private without being edited.

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

**Across replicas (#28).** The three steps above make a memory
searchable on the process that wrote it. A `refresh:` block inside
`memory:` makes it searchable on the *others*: each replica probes the
store's listing fingerprint on a schedule and, only when it moves,
re-`Load`s, rebuilds its overlay and its index off the request path, and
swaps both in atomically. A write that lands during that rebuild is
journalled and replayed into the new index before it is published, so a
save the caller watched succeed cannot be lost by a swap.

Two properties from this document survive that path by construction, and
are tested over it rather than only over a fresh mount. The rebuilt
overlay derives every page ID from the **store key** — never from the
document's `memory_namespace:` frontmatter — so a personal memory
re-read on another replica is private to exactly the principal who wrote
it. And the rebuilt index contains **every** document, private ones
included, because visibility is a clause in the query rather than an
index-time filter; a reload that filtered would hide a memory from its
own owner. See [hot-reload.md](hot-reload.md).

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
any page in the collection. Since #27 that ID is also the document's
**owner**: `memory/personal/<ns>/` is what every read surface derives
visibility from, which is why the store key having to win over the body
is now load-bearing twice over.

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

`memory_namespace` is **provenance, not policy**. It records whose
memory this is so a document copied out of the store is still
self-describing, but no read path consults it: visibility comes from the
page ID (see [The owner is the page ID](#the-owner-is-the-page-id)). A
document whose frontmatter claims somebody else's namespace is served
under the owner its store key says, and a test pins exactly that.

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

**#27's one deliberate behaviour change.** Personal memories written
before it stop being readable by anyone but their owner. That is the
point of the issue rather than a regression, and it is the only
direction the change goes: nothing becomes *more* readable. The
surfaces that keep their exact previous behaviour are worth listing,
because they are the ones an operator might worry about:

| surface | after #27 |
| --- | --- |
| `mk search` / `mk list` / `mk show` | unchanged — one local principal, unrestricted viewer |
| `mk http serve` (static token) | unchanged — untouched by this work, unrestricted viewer |
| `mk mcp serve` (stdio) | reads the `local` namespace, which is the only one it writes; a single-user deployment sees exactly what it saw |
| `team` / `global` memories | unchanged, at every scope and every surface |
| ordinary KB content | unchanged, except for the reserved `memory/personal/` prefix |
| an existing `memory:` block | keeps working; `personal_visibility` defaults to `private` |
| a deployment that wants the old reads | `personal_visibility: collection`, per collection |

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

### Private personal reads (#27)

- **The derivation** (`internal/kb/visibility_test.go`): which IDs are
  private and to whom, including truncated and near-miss prefixes, and
  that a document's frontmatter cannot claim an owner its ID does not
  have. Plus the distinction between `Unfiltered()` and `AsOwner("")`,
  which the registry's nil-viewer pointer exists to preserve.
- **The layout agreement** (`internal/memory/visibility_test.go`):
  `kb.PrivateOwner(ref.PageID) == ref.Namespace` for every scope and a
  battery of hostile subjects — the test that stops the two packages'
  notions of the layout drifting apart quietly. Also the read
  consequences of the namespace rules: two issuers with the same `sub`
  are two owners; changed email/groups/tenant move nothing; a different
  subject carrying the old claims inherits nothing.
- **The clause, not a post-filter** (`internal/search/visibility_test.go`):
  fifty private documents that all outrank the one public match, a limit
  of five, and the assertion that the public match comes back. It is the
  test that fails if the filter ever moves to the result loop, and it
  was confirmed to fail against exactly that implementation. Alongside
  it: score-for-score parity with an unrestricted query, the owner token
  being unreachable from free text, and restricted queries running
  concurrently with `Put`.
- **Absence, not refusal** (`internal/collections/visibility_test.go`,
  `internal/mcp/visibility_test.go`): a guessed ID — bare, qualified, or
  with an explicit `collection` — answered with the same bytes as a
  fictional one; the ambiguity error for a page in two collections
  reduced to a plain not-found for anyone who owns neither copy; page
  counts in `mk_list_collections` that do not move when another
  principal saves.
- **Both backends** (`internal/memory/visibility_test.go`): local and
  GCS driven through one table, asserting the store key — which is what
  the owner is derived from — survives the write/load round trip
  identically in each. Plus a remount at the collection level, which is
  the path a hot reload will take.
- **Concurrency**: several principals saving and reading through one
  shared `*Collection` at once, each asserting they never observe
  another's document, under `-race`.

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
  GCS. It would also be the missing half of a local `memory.refresh`,
  which #28 deliberately refuses today (see
  [hot-reload.md](hot-reload.md)): a directory one process owns has no
  second writer to converge with, and giving it one is this item.
- **Incremental reconciliation.** A moved fingerprint currently re-reads
  every live document. The listing already carries per-object
  generations, so re-reading only the objects whose generation changed is
  a contained improvement — it needs a `Store` method that takes the
  previous listing.
- **A memory-aware read tool.** Memories are found by `mk_search` and
  `mk_list --prefix memory/` today. A dedicated `mk_recall` that filtered
  to the caller's own namespace by default would be a better default
  than a prefix filter the model has to remember.
- **Team-private reads.** `team` still means "every reader of the
  collection". Scoping it to a server-derived team identifier would
  reuse the whole #27 mechanism — an owner token in the page ID, a
  clause in the query — but needs a decision about where a team identity
  comes from and what happens to a memory when somebody leaves one.
- **An administrative recovery capability.** #27's issue allows for an
  explicitly configured one; this implementation ships none. `admin`
  deliberately does not confer it (ownership is not a capability), so
  today the answer is the store itself, where the access is auditable.
  A capability that could read another principal's personal memories
  should probably also *log* every such read, and that is a design of
  its own.
- **Migrating an existing store.** A deployment that ran with
  collection-visible personal memories and wants them shared for real
  has no tool to promote one to `team`/`global` — that is the same
  missing promotion tool the review pipeline needs.
