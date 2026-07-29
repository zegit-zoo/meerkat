# OKF (Open Knowledge Format) bundles

meerkat can serve an [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
knowledge bundle directly — point `--kb-dir` or a `content-source.yaml`
at one, no conversion step. OKF is a directory of markdown files with
YAML frontmatter, which is already meerkat's own storage model.

- **Spec:** [SPEC.md](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md) (current version: 0.2)
- **Project:** [GoogleCloudPlatform/knowledge-catalog, `okf/`](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
- **License:** Apache-2.0

meerkat implements the OKF **consumer** side only — it reads bundles, it
does not produce them. meerkat is an independent, third-party consumer:
it is not affiliated with, endorsed by, or reviewed by OKF's authors
(Google Cloud Platform). OKF itself is early (v0.2) and is not a
ratified or industry-standard format — there is no governing body, and,
per SPEC.md §4.1, no central registry of `type` values. Everything
below describes what meerkat actually does with a bundle, not a claim
about OKF's own maturity or adoption.

## Serving a bundle

Per SPEC.md §3, a bundle's own root directory *is* the content root —
concept files (and an optional `index.md`/`log.md`) sit directly under
it, with no wrapping directory. meerkat's `--kb-dir` and
`content-source.yaml` instead resolve against the **content-repo
layout** (a `wiki/` subdirectory holding the pages — see the README's
["Serving content at runtime"](../README.md#serving-content-at-runtime)),
so point `layout.wiki` at the bundle root:

```yaml
# content-source.yaml
content:
  type: local
  path: /path/to/the-bundle   # the bundle root itself
  layout:
    wiki: "."                 # concepts sit directly under it
```

```bash
mk --content-source ./content-source.yaml list
mk --content-source ./content-source.yaml show tables/orders --json
```

Nothing inside the bundle changes — `content-source.yaml` lives outside
it and just names it.

`--kb-dir` on its own will **not** work for a bundle: it always assumes
the default layout (a `wiki/` subdirectory) and has nowhere to carry a
`layout:` override, so it finds no pages and reports an empty knowledge
base. Use `--content-source` as above.

The alternative that needs no `content-source.yaml` at all: copy or
move the bundle to `<some-dir>/wiki/` and point `mk --kb-dir <some-dir>`
at it directly — the same content-repo layout `mk ingest` already
writes into.

A bundle packaged as a `type: url` `.tar.gz` works the same way,
provided the archive was created **with** a wrapping top-level
directory (`tar -czf bundle.tar.gz bundle-dir/`, not `tar -czf
bundle.tar.gz -C bundle-dir .`) — extraction preserves entry paths
verbatim, so `layout.wiki` names that top-level directory after
extraction, same as `type: local` above. See
[content-source.example.yaml](../content-source.example.yaml) for the
required `sha256`, and the README's
["`type: url`"](../README.md#type-url) section for how the extracted
result is cached.

## Reserved filenames

OKF reserves `index.md` and `log.md` at every directory level, for
directory listings (§8) and change history (§9) respectively — neither
is a concept document. meerkat skips them, but the skip is keyed on
**whether the file has a frontmatter block**, not on the filename:

| File | Frontmatter | Result |
|---|---|---|
| `index.md` / `log.md` | none | skipped — OKF navigation artifact |
| `index.md` / `log.md` | present | indexed like any other page |
| any other `.md` file | — | always indexed |

This is deliberate, and worth preserving if you're tempted to key it off
the filename instead: an OKF `index.md`/`log.md` carries no frontmatter
at all (§8, §9), but meerkat's own knowledge bases have used `index.md`
as an ordinary, frontmatter-bearing landing page (`id:`, `title:`) since
before OKF existed. Matching on filename alone would silently stop
indexing every existing meerkat KB's `index.md`. Matching on frontmatter
presence instead lets both conventions coexist untouched.

One edge case falls out of this on purpose: SPEC.md §12 allows a
bundle-root `index.md` to carry a frontmatter block containing only
`okf_version` — the one exception to "index.md has no frontmatter."
Such a file *is* indexed as an ordinary page (title taken from its first
heading, `okf_version` preserved under `extra`) rather than skipped. No
published OKF sample bundle sets `okf_version` today, so there was
nothing to validate different handling against; if that changes, this is
the behavior to revisit.

## Frontmatter mapping

| OKF field | meerkat handling |
|---|---|
| `title`, `tags` | Native — the same core fields meerkat already had. |
| `type` | Promoted to a core field — OKF's only required key (§4.1). Also a filter facet; see below. |
| `description` | Promoted to a core field — OKF's recommended one-line summary (§4.1). |
| `generated`, `verified`, `stale_after` | Promoted — the trust/lifecycle family; see below. |
| `resource`, `sources`, `okf_version`, `runtime`, `parameters`, `computation`, `executor`, `attester`, and any other producer-defined key | Preserved verbatim under `front.extra` — this is what OKF requires of consumers (§4.1, §11). |

meerkat's own `source` (singular — host/repo/ref/path, meerkat's
ingestion provenance) predates OKF and is a different key from OKF's
`sources` (plural, §5.1); the two do not collide.

## Trust and lifecycle

`generated`, `verified`, and `stale_after` (§5.2, §5.5) surface on every
read surface, alongside the page's own frontmatter:

- `mk show <id> --json`
- MCP `mk_show`
- HTTP `POST /show`

Each adds two fields computed on the fly, not stored: `trust_tier` and
`stale`.

`trust_tier` is derived from `verified`:

| `verified` | `trust_tier` |
|---|---|
| absent | `unverified` |
| present, no entry's `by` starts with `human:` | `machine-confirmed` |
| present, at least one entry's `by` starts with `human:` | `human-reviewed` |

A bare `verified: { by: ..., at: ... }` mapping (no list dash) parses
the same as a one-element list, per §5.2/§11.

`stale` is `true` once today is on/after `stale_after` (an absolute
`YYYY-MM-DD` date, not a relative TTL — §5.5); an absent or unparsable
`stale_after` is never stale.

```
$ mk show tables/orders --json
{
  "id": "tables/orders",
  "front": {
    "type": "BigQuery Table",
    "verified": [{"by": "human:ahormati", "at": "2026-06-25T09:00:00Z"}],
    "stale_after": "2099-01-01",
    ...
  },
  "trust_tier": "human-reviewed",
  "stale": false
}
```

`trust_tier` is advisory metadata the bundle's producer asserted, not
something meerkat checks — see
[SECURITY.md](SECURITY.md#okf-trust_tier-is-advisory-not-verified).

## `type` as a filter

`type` (OKF's required concept-kind field — `BigQuery Table`, `Metric`,
`Playbook`, …) composes (AND) with meerkat's existing
prefix/category/status/owner filters, on all three surfaces:

```bash
mk list --type "BigQuery Table"
```

```jsonc
// MCP mk_list argument, or the HTTP POST /list body
{"type": "BigQuery Table"}
```

There is no central registry of `type` values (§4.1) — matching is an
exact string comparison against whatever a given bundle happens to use.

## `status` vocabularies coexist

OKF's `draft | stable | deprecated` (§5.4) and meerkat's own
`placeholder | reviewed | stale | ingest-failed | needs-research |
superseded` both pass through the `status` field untranslated — nothing
in meerkat assumes a closed set, so `mk list --status draft` filters an
OKF bundle exactly the way `mk list --status placeholder` filters a
meerkat KB.

One default is *not* carried over: SPEC.md §5.4 says an absent `status`
means `stable`. meerkat applies no such default — a concept with no
`status` key reports `status: ""`, and `--status stable` will not match
it; only an explicit `stable` does.

`mk ingest`'s default filter only ever selects `status: placeholder` /
`ingest-failed` (see [INGESTION.md](INGESTION.md)) — none of OKF's own
status values are in that set, so OKF-authored pages are never picked up
by the ingestion pipeline as work to do.

## What isn't implemented

- **meerkat does not produce OKF.** There is no export/generate path,
  only consumption.
- **No cross-link resolution.** Markdown links between concepts
  (bundle-relative `/tables/customers.md`, or relative `./orders.md` —
  §6.1) are left as plain markdown text; meerkat does not validate,
  rewrite, or follow them, so a broken link has no effect on indexing or
  serving.
- **The Attested Computation family is inert.** `type: Attested
  Computation` concepts and their `runtime`, `parameters`, `computation`,
  `executor`, `attester` fields (§10) are preserved under `front.extra`
  like any other unrecognized keys. meerkat does not execute a
  computation, run an attester, or check a receipt.

## Conformance fixture

`internal/kb/testdata/okf-bundle/` (exercised by
`internal/kb/okf_test.go`) is a small hand-written bundle covering the
full frontmatter set, a required-field-only concept, reserved files with
and without frontmatter, a bare `verified` mapping, human and non-human
verifiers, and a broken cross-link.
