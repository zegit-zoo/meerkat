# Links, pointer pages and capability bundles

meerkat-mob issue B (#2). Until this change `related:` was parsed and
never read, and a collection could not say "the answer lives over
there". The mob design is a tree of knowledge bases; without resolved
links there is no tree, only a list. This page records what `related:`
now means, what a pointer page is, how search ranks and groups them,
and where the seams are.

## Link syntax

A `related:` entry, or a pointer's `target:`, is one of (`kb.ParseLink`):

| Form | Kind | Resolves against |
|---|---|---|
| `<id>` | local | the page's own collection |
| `<collection>:<id>` · `page:<collection>:<id>` | qualified | the named mounted collection |
| `collection:<name>` | collection | the mounted set (pointer targets only) |
| `ext:<scheme>:<target>` · `external:<name>` · `<scheme>://…` | external | nothing — never checked, never dangling |

A collection name is `[A-Za-z0-9][A-Za-z0-9_-]*` (the same rule
`content-source.yaml` enforces) and never contains `:`, a page ID may
contain `/` and `.` and never `:`, so `<collection>:<id>` is
unambiguous. `mcp://<server>` is the MCP protocol's own vocabulary for
an external server (decision Q9, 2026-09-17); the pointer page's
`tools:`, `resources:`, `prompts:` and `toolsets:` list that server's
capabilities in the same terms. Auth is never in a page.

## Page types

Three `type:` values have routing semantics; every other value is an
OKF concept kind and means nothing to the router.

- **`pointer`** — `target:` (collection, page or external) and `hint:`
  (one sentence, ≤ 300 characters, telling an agent why to go). Body
  optional. A pointer whose target is a page in its *own* collection is
  refused: that is a `related:` entry, not a hop.
- **`skill`** — a how-to that belongs with a pointer (linked from it
  through `related:`).
- **`example`** — a worked example, bundled the same way.

`kb.Page.Pointer()` validates the shape; `collections.KindOf` maps a
page to `pointer | skill | example | doc`.

## Resolution

`internal/collections/links.go` builds a **link graph** over every page
the registry holds — content and memory overlay alike — once per
configuration of the mounted set, keyed by each collection's snapshot
version and overlay generation. A reload, a memory save or a memory
reconciliation invalidates it exactly once; a `mk_show` is a map
lookup, never a walk. Views (`Restrict`, `ViewedBy`) share the root's
graph and filter at read time, so a per-request authorized view never
rebuilds anything.

Per page (`Registry.LinksOf`):

- `links` — each `related:` entry with `resolved` and, when not, a
  `reason` (`collection "x" is not mounted`, `page "y" not found in
  collection "x"`).
- `invalid_links` — entries that did not parse, with the reason.
- `linked_from` — qualified IDs of pages whose `related:` names this
  one, **filtered by the viewer**: a private memory page that links to a
  public page never reveals itself through the public page.
- `pointer` / `pointer_error` — for a pointer page, its resolved target,
  or why it is not a valid pointer.

Resolution status is viewer-independent: whether `flux:drift` exists
does not depend on who asks, and the ID was already in the source
page's own frontmatter.

### Dangling is a warning, never an outage

`Registry.LinkReport()` lists every unresolved or invalid entry.
`Registry.Check()` folds each collection's share into its `Health` as
`dangling_links` (exact count) and `warnings` (the first five);
`/readyz` stays ready. A broken link is a content bug to fix in the
content repository, and the tool for that is:

```
mk lint          # prints every dangling entry, exit 1 if any
mk lint --json   # {pages, links, dangling: [{collection, page, link, reason, pointer}]}
```

`mk lint` mounts the configured collections exactly as `mk search`
would, so a content repo's CI can refuse to publish a dangling link.
It is the librarian agent's first tool (issue H).

## Ranking

`search.WithTypeBoosts` is the `type` counterpart of
`WithCategoryBoosts`, with one deliberate difference: it is a
**multiplier on the final score**, not an added clause. An added clause
cannot reliably lift a two-line pointer page above a content page whose
title matches every term, and "the hub's routing pages outrank its own
thin content" is the property the design needs. The rule is legible: a
typed page wins whenever its own match is within 1/boost of the best
content hit. Defaults (`search.DefaultTypeBoosts`): pointer ×4, skill
×2, example ×1.5 — starting points to be tuned from retrieval telemetry
(issue F), not constants. An empty map disables the boost.

## Wire shapes

Every search hit (`mk_search`, `POST /search`, `mk search --json`)
gains `type`, `kind` and — for a pointer — `target`, `target_resolved`,
`hint`. Every page view (`mk_show`, `POST /show`, `mk show --json`)
gains `kind` and the `links` / `invalid_links` / `linked_from` /
`pointer` / `pointer_error` fields above. All additive; nothing was
renamed.

`mk_search` with `bundle: true` returns the same hits grouped as
**capability bundles** — the tier-0 answer shape Datadog's capability
search is the model for:

```json
{"query": "…", "bundles": [
  {"target": "collection:flux", "hint": "GitOps, HelmRelease drift.", "score": 1.9,
   "pointers": [...], "skills": [...], "examples": [...], "docs": [...]},
  {"target": "", "docs": [...]}
]}
```

A pointer hit opens the bundle for its target; every other hit joins
the bundle of the first pointer whose `related:` names it, and
otherwise the unrouted bundle (`target: ""`). Nothing a search found is
dropped. Bundles are ordered by their best hit.

## Telemetry

The show span carries `meerkat.show.links` and
`meerkat.show.linked_from` — counts. A link target is a page ID, and
page IDs never reach a span; the disclosure rule is unchanged.

## Not in this change

- Following a pointer automatically (an agent hops by calling
  `mk_search` on the target collection; issue D adds the tree manifest
  that makes the hop explicit and bounded).
- Per-collection boost configuration in `content-source.yaml`; the
  option exists in code and can be plumbed when a deployment needs it.
- Markdown links inside page bodies (`[x](./orders.md)`, OKF §6.1)
  remain plain text; only frontmatter links are resolved.
