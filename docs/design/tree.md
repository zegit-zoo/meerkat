# The knowledge-base tree: manifests, depth guard, cold children

meerkat-mob issue D (#4). Until this change the mounted set was a flat,
ordered list of collections. The mob design is a **tree**: a thin root
that only routes, hubs that route further, and leaves that hold the
pages — with any page reachable from the root in at most five hops.
This page records the manifest, the loader's guards, what the registry
does differently in tree mode, and what is deliberately left to the
next issues.

## `manifest.yaml`

Every knowledge base in a tree carries one at its root (the resolved
content directory, beside `wiki/`):

```yaml
kind: KnowledgeBase
name: platform              # the collection name; unique across the tree
tier: 1                     # optional, informational; must match the real depth (root = 0)
parent: root                # optional, informational; must match the real parent
description: Platform engineering hub
children:
  - name: flux
    source: {type: s3, bucket: kb, prefix: kb/flux/, endpoint: https://s3.example.net, region: garage, path_style: true}
    mount: eager            # eager (default) | lazy
  - name: vendors
    source: {type: gcs, bucket: kb, prefix: kb/vendors/}
    mount: lazy
size_hint_bytes: 1200000
stale_after: 2026-12-31
contract: {method: merge-request, repo: zegit-zoo/kb-platform, path: wiki/}
placement: shared           # shared (default) | dedicated
access: {paths: [root/platform], identities: [team-platform]}
limits: {max_hops: 12, max_steps: 40, max_attempts: 20}
depth_limit: 3              # optional; lowers the cap for this subtree, never raises it
```

`children[].source` is an ordinary content source — anything
`collections:` accepts (S3, GCS, url, local). `contract:` is the
collection's update contract and is applied unless the child source
declares its own. Unknown keys are refused.

`content-source.yaml` names the root:

```yaml
tree:
  type: s3
  bucket: kb
  prefix: kb/root/
  endpoint: https://s3.example.net
  region: garage
  path_style: true
```

`tree:` is exclusive with `content:` and `collections:`; the tree's
members come from its manifests.

## The walk (`contentsource.ResolveTree`)

Depth-first, manifest order. Each eager child is resolved exactly as a
`collections:` entry would be — same cache, same conditional reads,
same hot-reload seams — then its manifest is read and its children
follow. The result is the flat list `collections.Open` already takes
(root first), with each collection carrying its `TreeNode` (name,
path, depth, parent, children, mount, placement, access, limits).

Guards, all load errors with a message that names the offending path:

| Guard | Rule |
|---|---|
| depth | a KB deeper than 5 below the root (root = 0) is refused — the hard cap from Q2. `depth_limit` may lower it for a subtree. |
| cycles / duplicates | a child whose source location was already walked, or whose name was already declared, is refused. Names are collection names and address pages (`<collection>:<id>`), so they must be unique across the whole tree. |
| agreement | a child's `name` must equal the name in its own manifest; `parent:` and `tier:`, when set, must match the real position. |
| shape | `kind: KnowledgeBase`, a valid collection name, a valid `mount`/`placement`, an ISO `stale_after`, non-negative limits, and a source that validates. |

## Cold children

`mount: lazy` children are **declared but not mounted** at startup:
they appear in `mk_list_collections`, `GET /collections` and `mk list
--collections` with `mounted: false`, `source: cold` and no pages, so
an agent knows the name exists. The first request that names one
mounts it under the cache's cold policy — blocking by default, or an
immediate `{status: cold, retry_after_ms}` answer with `cold_policy:
async` — and the resident budget decides how long it stays warm. See
[docs/design/cache.md](cache.md).

## What the registry does differently in tree mode

- **An unqualified search asks the root hub only.** The root's job is
  to route; its pointer hits (issue B) name the next hop. A flat
  deployment still fans out over every collection. A view that cannot
  see the root falls back to what it can see.
- **Tree paths are collection aliases.** `root/platform/flux` names the
  same collection as `flux`, in `mk_search`'s `collection` and in a
  qualified page ID (`root/platform/flux:concepts/drift`).
- `mk_list_collections` (and the HTTP and CLI listings) carry `path`,
  `tier`, `parent`, `children[] {name, mount, mounted, source_type}`,
  `mounted`, `mount`, `placement`. A restricted view lists only the
  subtree it can see.
- `meerkat.tree.depth` on the list-collections span and the
  `meerkat_tree_depth` gauge (domain telemetry, so an unconfigured
  server's `/metrics` is unchanged) report the deepest declared KB.

`placement`, `access` and `limits` are carried, validated and exposed;
enforcing them is issue J (operator), the authz layer, and issues F/G
(session limits and the `limit_reached` response) respectively. The
root's effective limits (`DefaultLimits` where unset: 12 hops, 40
steps, 20 attempts) are available as `Registry.TreeLimits()`.

## Readiness

A cold child never affects `/readyz`: it has no collection to be
unready. The root and every eager child must enumerate and index, as
before.
