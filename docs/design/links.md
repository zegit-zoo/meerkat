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
version, snapshot generation and overlay generation. A reload, a lazy
mount or eviction, a memory save or a memory reconciliation invalidates
it exactly once; a `mk_show` is a map lookup, never a walk. A rebuild
re-lists a collection's content root only when that collection's
snapshot changed. Each collection's content pages are cached per
snapshot, so the rebuild after a memory save merges the live overlay
over cached content and reads nothing from disk or the object store.
The cache holds nothing for a collection that is cold or no longer in
the registry. Views (`Restrict`, `ViewedBy`) share the root's
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

Resolution status is decided per view. In a restricted view, a link
into a collection the caller cannot read gets `collection "x" is not
mounted`, and a link to a page the caller's viewer may not see (another
principal's personal memory) gets `page "y" not found in collection
"x"`. Both read word for word like a link to something that does not
exist. A pointer's `pointer` block and a search hit's `target_resolved`
follow the same rule, and `PointersTo` lists only pointer pages the view
can see. The link text sits in a page the caller can read, but whether
its target exists is still about the target, which may be hidden.
`LinkReport` (`mk lint`, `/readyz` health) stays an operator view over
the whole mounted set.

### Dangling is a warning, never an outage

`Registry.LinkReport()` lists every unresolved or invalid entry.
`Registry.Check()` folds each collection's share into its `Health` as
`dangling_links` (exact count) and `warnings` (the first five);
`/readyz` stays ready. A broken link is a content bug to fix in the
content repository, and the tool for that is:

```sh
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

The defaults are right for a **hub** and wrong for a **leaf**, so they
apply by ROLE (#95), which meerkat reads from the deployment's shape
(`Collection.IsHub`):

- **Hub**: a small tier of routing pages above thin content, each
  pointer a hop to another collection or an MCP server. That is the
  root of a `tree:` deployment, or a collection that declares a hub tier
  with `layout.analyzer: ngram`. It ranks with the defaults, because the
  pointer outranking the page is the point.
- **Leaf**: everything else. A single collection, a flat `collections:`
  entry, a tree child. It is a content collection whose pointers, when it
  has any, are citations (one per book chapter or article). A citation is
  a reference, not a route, and ×4 would let it outrank the page that
  answers the question. So a leaf ranks with no type boost.

The role is never taken from how many collections are mounted: a tree
is always several collections, and its root is exactly the one that
keeps ×4. A collection states its own weights in `content-source.yaml`,
and they replace the role's defaults for that collection only, in either
direction:

```yaml
    search:
      type_boosts: {pointer: 4, skill: 2, example: 1.5}   # a flat hub keeps the ×4
```

Before #95 every collection defaulted to the hub weights, and each leaf
opted out with `{pointer: 1.0}` (#88). That setting is now the leaf
default, and harmless to keep. A pointer's `hint:` is indexed too, at the body's
weight (`docs/SEARCH.md`), so the one sentence that says what is at the
other end is searchable on either shape.

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
- Deriving the default from the pointer's target — boosting only a
  pointer that hops out of its own collection. `type_boosts` makes the
  choice explicit per collection instead (#88).
- Markdown links inside page bodies (`[x](./orders.md)`, OKF §6.1)
  remain plain text; only frontmatter links are resolved.
