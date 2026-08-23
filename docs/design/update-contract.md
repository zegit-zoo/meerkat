# Spec: Per-collection update contract — the sanctioned contribution path

**Status:** Implemented at the config + registry layer (`update:`/`description:` in content-source.yaml, `internal/contentsource`, `internal/collections`); **discovery surfacing is pending** — see [Surfacing](#surfacing-mk_list_collections) · **Builds on:** [memory.md](memory.md), [hosted-mcp.md](hosted-mcp.md) · **Issue:** #23

*(`docs/design/` is a design record. The up-to-date schema reference is
[content-source.example.yaml](../../content-source.example.yaml); the
user-facing reference is the [README](../../README.md).)*

## Summary

A collection tells a caller everything about how to *read* it and nothing
about how knowledge gets back *in*. An agent that learns something during
a session — a decision, a correction, a convention nobody wrote down —
has no discoverable, sanctioned way to contribute it. It can guess, and
the guesses are all bad: write into the served directory (which is a
read-only mirror, or a cache that the next content resolution replaces),
open a pull request against whatever repository it can find (which may be
the wrong one, or the mirror rather than the source), or do nothing.

Today that knowledge lives in operators' heads, and an agent has no way
to ask.

This spec adds an operator-declared **update contract** per collection: a
`method:` (`direct`, `merge-request`, or `none`), the mechanics a
merge-request flow needs, and free-text `instructions:` — an agent-facing
skill covering fork-vs-branch policy, page format, and what to run before
proposing. Plus an optional `description:`, because a collection *name*
is an identifier, not an explanation.

Two properties carry the design:

1. **The contract is declared, never inferred.** A serving mirror and a
   contribution repo are different addresses, and only the operator knows
   which one takes contributions.
2. **What a caller is shown is what that caller can actually do.** A
   contract that tells someone to do something they will be refused for
   is worse than no contract at all — it costs a round-trip and teaches
   the model something false.

## Non-goals

- **meerkat opening merge requests.** The contract is *instructions*.
  meerkat never clones, never pushes, never talks to a forge. The agent
  does that with its own credentials, which is also why the
  merge-request path needs no meerkat capability at all.
- **Performing direct writes.** `method: direct` describes an existing
  capability (the memory toolset, a mounted volume, a bucket the agent
  can reach); it does not add a write surface. The only writes meerkat
  performs are still `mk_save_memory`'s — see [memory.md](memory.md).
- **Surfacing it through MCP/CLI/HTTP.** The registry API is complete and
  tested; wiring it into collection discovery lands with
  `mk_list_collections` (#22). See [Surfacing](#surfacing-mk_list_collections).
- **Per-page contracts.** The collection is the unit here, as it is for
  read access and for memory.
- **Validating that the contribution repo exists.** Startup does no
  network I/O for a contract. An unreachable repo is an operator error
  that a failed `git clone` reports, not a reason to refuse to serve.

## The taxonomy

| `method:` | means | who it is for |
| --- | --- | --- |
| `direct` | write into the collection's own backend; the write *is* the update | a bucket prefix or directory the deployment treats as live, writable knowledge |
| `merge-request` | open a merge/pull request against the **contribution repo** and let a human review it | a git-authoritative knowledge base — the common case for anything durable |
| `none` (default) | there is no sanctioned contribution path | read-only mirrors, vendor documentation, anything an operator has not thought about yet |

`none` is also what an absent `update:` block means, which is what every
configuration written before this feature has. Nothing changes for them,
and nothing new is offered on their behalf.

### Why `merge-request` is the interesting one

A knowledge base worth protecting is reviewed. The whole point of a
review flow is that a contribution is a *proposal* until a human agrees,
and the forge already has the machinery: diffs, comments, CI, an audit
trail, and a revert. Reproducing any of that inside meerkat would be
worse at it. So the contract's job is to hand an agent the four facts it
cannot derive — **repo**, **host**, **branch**, **path** — plus the
operator's own prose, and then get out of the way.

`host:` (`github` | `gitlab` | `other`) exists for exactly one reason: it
tells an agent which CLI mechanics to reach for (`gh pr create`,
`glab mr create`, or neither). It is not a network endpoint; meerkat
never contacts it.

## Declared, not inferred

The tempting shortcut is to derive the contract from the source type:
`type: local` is a directory, so it must be directly writable; `type:
git` has a repo, so that must be where PRs go.

Both inferences are wrong in the deployments that matter most.

- The `type: gcs prefix:` a collection is served from is very often a
  **mirror** — a bucket some pipeline syncs from a git repository that
  the serving process cannot reach and should not name. Contributions go
  to the repo. Nothing about the GCS address says so.
- A `type: local` path is just as often a **checkout an operator does not
  want written to**, or a directory rebuilt by a deploy. A write into it
  looks like it worked and disappears at the next release.
- Conversely, a repo an agent can *find* is not necessarily the repo it
  should *propose to*: forks, monorepos, and "docs live in this other
  repository" are all normal.

So the operator declares. The one place meerkat reasons about the source
type at all is a **rejection**:

> `method: direct` needs a backend a write can land in. A local
> directory or a GCS object prefix qualifies — the same notion of
> "writable" a `memory:` store uses (see `internal/memory.Spec`).
> A digest-pinned `type: url` archive, a `type: gcs object:` bundle,
> `type: none` (content embedded at build time) and the build-time-only
> `type: git`/`type: submodule` sources do not, and the contract is
> refused **at config load**.

That inference can only ever *reject* a declaration, never invent one.
And it fails at startup, where an operator sees it, rather than at the
first write — a contract that promises a direct write into a
content-addressed cache is data loss discovered days later by whoever
believed it.

A `merge-request` contract, by contrast, is legal on **every** backend
including `type: none`. That asymmetry is the design: the serving address
and the contribution address are unrelated by construction.

## Config schema

```yaml
collections:
  - name: handbook
    type: gcs                    # a mirror; the source of truth is the repo below
    bucket: my-org-knowledge
    prefix: handbook/live/
    description: Engineering handbook — conventions, onboarding, how we work.
    update:
      method: merge-request
      repo: https://github.com/example-org/handbook.git   # or git@…:…, ssh://…
      host: github               # github | gitlab | other   (default: other)
      branch: main               # default: main
      path: wiki                 # where pages live in the CONTRIBUTION repo
      instructions: |
        Fork example-org/handbook to your own account; we do not take
        branches on the upstream repo. One page per pull request.
        …

  - name: scratch
    type: local                  # a writable directory this deployment owns
    path: ../scratch
    description: Working notes. Anything here may be edited in place.
    update:
      method: direct
```

Both keys are valid under `content:` too — a single-source config
resolves to one collection named `default`, and it gets a contract like
any other.

| key | required | notes |
| --- | --- | --- |
| `description` | no | ≤ 500 characters. It is rendered into an agent's context every time collections are listed; long-form guidance belongs in `instructions`. |
| `update.method` | **yes, if the block is present** | `direct` \| `merge-request` \| `none`. Not defaulted: a block whose method could be inferred would let a typo'd key silently mean "none". |
| `update.repo` | for `merge-request` | `https://`, `ssh://`, or `git@host:owner/repo`. An `owner/repo` slug is refused — it names no host, and the contribution repo need not be on the host the content is served from. |
| `update.host` | no | `github` \| `gitlab` \| `other`. Defaults to `other`. |
| `update.branch` | no | Defaults to `main`. |
| `update.path` | no | Repo-relative. Deliberately *not* defaulted from `layout.wiki`: that describes the serving mirror. |
| `update.instructions` | no | Free-text, multiline. Allowed for `direct` too (a directly-writable collection still has a page format), refused for `none` (there is no path to describe). |

**Misplaced fields are errors, not ignored lines.** `repo:` under
`method: direct` is refused, as is `instructions:` under `method: none`.
An operator who wrote a key believes it does something; silently dropping
it makes the contract meerkat renders differ from the one they read — the
same call `auth:` and `memory:` already make about unknown capabilities
and mismatched backend fields.

**A repo URL may not embed credentials.** `https://user:token@host/…` is
refused at load. The contract is rendered to *every caller who can see
the collection*, so a token in that URL is a token handed to all of them.
Refusing it at startup beats redacting it on one surface and forgetting
the next. (An `ssh://git@host/…` login name is not a secret and is fine.)

## Effective rendering

The declared contract is the same for everybody. What a caller should
*do* is not, so `Collection.EffectiveContract(grants)` renders it against
that caller's capabilities. The result carries both — `method` (what to
do) and `declared_method` (what the operator wrote) — because "you may
open a merge request" and "you may open a merge request *because you
cannot write here directly*" are different messages, and an agent that
can see both can tell a user which one it is looking at.

### The ladder

Evaluated in order; the first rung that applies wins.

| # | rung | applies when |
| --- | --- | --- |
| 1 | `direct` | declared `direct` **and** the caller holds `global-write` (or `admin`) on the collection |
| 2 | `merge-request` | declared `merge-request` — no capability gate |
| 3 | `staging` | a declared contract the caller cannot take, **and** the collection has a `memory:` store, **and** the caller holds any write capability there |
| 4 | `none` | everything else, with a reason saying which |

### The decision table

| declared | caller holds | effective | reason, in short |
| --- | --- | --- | --- |
| `direct` | `global-write` / `admin` | **`direct`** | may publish here |
| `direct` | `personal-write` or `team-write`, memory store configured | **`staging`** | a writer, not a publisher — propose it with `mk_save_memory` and it is parked as a pending review artifact |
| `direct` | `read` only | **`none`** | lacks the capability, and there is no proposal route |
| `direct` | any, **no** memory store, no publish capability | **`none`** | nowhere to stage |
| `merge-request` | anything, including `read` only | **`merge-request`** | the forge is the authority, not meerkat |
| `merge-request` | `admin` | **`merge-request`** | capabilities never *widen* a declaration |
| `none` | anything | **`none`** | the operator declared no path |
| any | **no grants in force** (nil) | **the declared contract** | there is no capability gate to fail |

Three rules are worth spelling out, because each of them is a decision
that could plausibly have gone the other way.

**`global-write` is what gates `direct`.** It is the same capability the
memory toolset requires for a *global* memory, for the same reason: a
direct update to a collection's content is immediately visible to every
reader of that collection. A caller holding only `personal-write` is a
writer, not a publisher, and telling them to write into the knowledge
base directly would be advice that either fails or — worse — succeeds.
`admin` implies it, as `admin` implies everything.

**The merge-request rung has no capability gate.** Opening a pull request
uses the *contributor's* forge credentials against a repo meerkat does
not serve and cannot police. There is nothing here to check. A caller who
should not even know the collection exists never reaches this rung: the
collection is not in their registry (below).

**Capabilities can only walk a caller *down* the ladder.** An admin on a
`merge-request` collection is still told to open a merge request. The
operator declared a review flow; a capability model that let privilege
route around it would make the declaration advisory, which is exactly
what a "sanctioned path" must not be.

### The staging rung, and why `none` stays `none`

A caller who may write in a collection but not publish to it is not out
of options: the memory toolset stages an unauthorized team/global write
as a pending review artifact under `<store>/_staging/`, which is a
proposal a human promotes (see [memory.md](memory.md)). That is precisely
the fallback a `direct` contract needs when the caller lacks
`global-write`, so the ladder uses it.

It is offered **only as a fallback from a contract that was declared**. A
collection declaring `method: none` renders `none` even when it has a
memory store and the caller could write to it: `none` is a *statement*,
not an absence of information, and answering "well, stage it anyway"
would render a contract the operator did not write. (The memory tool
advertises itself on its own terms — it is offered, or not, by the tool
filter — so nothing is hidden by this; the contract simply declines to
speak for it.)

### Anonymous / local mode

The CLI, `mk mcp serve` on stdio, `mk http serve`, and a hosted server
with no `auth:` block all run with **no policy in force** — a nil
`*authz.Grants`. `Grants.Can` answers `true` for everything in that
state, so every rung's capability check passes and **the effective
contract is the declared contract, unchanged**. There is no local
capability gate to fail, and pretending there is one would tell a
single-user CLI it may not contribute to its own knowledge base.

This falls out of the existing nil-grants semantics rather than being
special-cased, which is the point: one code path, and the back-compat
behaviour is the behaviour of the code that has no policy to apply.

## Invisibility: a contract is registry-shaped

A contribution repo URL is not public information. Neither is a
collection's description, nor an operator's instructions ("ask the
compensation committee first" says quite a lot about a collection called
`secrets`).

So the contract API deliberately hangs off `*Collection` and `*Registry`
and **nowhere else**:

```go
func (c *Collection) Description() string
func (c *Collection) Contract() Contract                                   // declared
func (c *Collection) EffectiveContract(g *authz.Grants) EffectiveContract  // rendered
func (r *Registry) EffectiveContracts(g *authz.Grants) []EffectiveContract
func (r *Registry) EffectiveContract(name string, g *authz.Grants) (EffectiveContract, error)
```

There is no package-level function that maps a collection name to a
contract, and no accessor that reaches past a view to the mounted set.
`Registry.Restrict` — [#9](hosted-mcp.md)'s single point of authorization
— therefore covers this feature completely, with no new enforcement:

- `EffectiveContracts` iterates `r.list`, which on a restricted view *is*
  the caller's visible set. A hidden collection's contract is not
  filtered out of the result; it was never a candidate for it.
- `EffectiveContract(name, …)` resolves through `Registry.Get`, so asking
  for a hidden collection produces the identical `ErrUnknownCollection`
  message a name nobody ever mounted produces.
- Every `reason` string names only *this* collection and the *caller's
  own* capabilities over it — the same disclosure the memory toolset's
  refusal already makes to somebody who can see the collection.

A test asserts that a restricted view exposes no trace of a hidden
collection's contract — not the repo URL, not the description, not the
instructions, not the name — anywhere in the rendered result.

## Surfacing (`mk_list_collections`)

This layer is deliberately surface-free. The intended consumer is
collection discovery (#22): `mk_list_collections` returns, per collection
the caller can see, its name, type, provenance, page count, `description`,
and its **effective** contract — so an agent that has just been told
"there are three collections" is told in the same breath what each one is
for and how to contribute to it.

The integration is one call: build the caller's restricted registry (the
handler already has it), then `reg.EffectiveContracts(authz.FromContext(ctx))`.
Because the rendering is effective rather than declared, the tool output
differs per caller by construction — which is also why it must not be
cached across sessions.

The same rendering is the natural body of a future `mk_contribute`-style
tool ("how do I add this?"), and of `mk list --collections`, where the
nil-grants path renders the declared contract for a single-user CLI.

## Testing strategy

- **Config** (`internal/contentsource/update_test.go`): all three methods
  parse with their defaults (`branch: main`, `host: other`, trailing-slash
  and case normalisation); `description:` on both config shapes; an
  absent block reads as `none`; and the whole rejection matrix —
  missing/unknown method, a merge-request with no repo, an `owner/repo`
  slug, a plain-`http://` repo, a repo with an embedded token, an unknown
  host, an absolute or `..`-bearing in-repo path, merge-request fields
  under `direct`/`none`, `instructions` under `none`, an over-long
  description, and a named collection naming the offending path.
- **The direct-on-a-read-only-backend rejection** gets its own table over
  every backend: `local` and `gcs prefix:` accepted; `url`, `gcs object:`,
  `none` and `git` refused, with the error asserted to name the backend
  *and* point at `merge-request` — an unactionable rejection just gets
  the block deleted. The mirror image is pinned too: `merge-request` is
  accepted on every one of those backends.
- **Effective rendering** (`internal/collections/contract_test.go`): each
  rung of the ladder, `admin`-implies-`global-write`, capabilities never
  widening a declaration, `none` staying `none` under a memory store,
  nil grants rendering the declared contract, and the acceptance
  criterion itself — three grant shapes over one collection producing
  three different effective contracts from one declared one.
- **Invisibility**: a restricted view's rendered contracts are searched
  for every string belonging to the hidden collection, and its
  by-name lookup is compared character-for-character against an absent
  collection's error.

## Future work

- **Surfacing** through `mk_list_collections` / `mk list --collections`
  (#22), as above. Nothing in this layer is user-visible until then.
- **A `mk contribute` command** that executes the merge-request contract
  — clone, branch, write the page, run the validation the instructions
  name, push, open the PR — reusing `internal/auth`'s forge credentials.
  That is the point at which `host:` stops being advice and starts being
  behaviour, and it wants its own threat review: it is meerkat pushing to
  a repository on a caller's behalf.
- **Promoting a staged memory into a merge request.** [memory.md](memory.md)
  parks proposals in a directory and calls a real review pipeline future
  work; a collection that declares a merge-request contract is exactly
  the deployment that would want the staged document turned into a PR
  instead.
- **Templates.** `instructions:` is prose because prose is what an
  operator can write today. A structured page template (frontmatter
  requirements, required sections) would let an agent be checked rather
  than trusted, and `layout.templates` already exists to hold one.
- **Contract inheritance.** Every collection in a deployment often has
  the same contract; a top-level default that entries override would cut
  the repetition. Deliberately not built yet — a wrong contract inherited
  silently is worse than a right one written twice.
