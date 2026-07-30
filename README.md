# meerkat — the vigilant guard and informer

Part of the [zegit](https://zegit.dev/) platform — meerkat *knows*.
Full documentation: [zegit.dev/documentation/meerkat.html](https://zegit.dev/documentation/meerkat.html).

Single-binary CLI that bundles a knowledge base and exposes it via:

- **CLI** — `mk search`, `mk show`, `mk list`, `mk ingest`
- **MCP** server (Model Context Protocol) — for agent harnesses / OpenCode / Claude Desktop
- **HTTP/OpenAPI** server with bearer-token auth — for OpenWebUI

The wiki is **embedded into the binary at build time** so search/show/list
work offline with zero runtime dependencies. The body of content and its
ingestion sources are configuration — point meerkat at your own content
and sources.

> **Early development — pre-1.0.** This project is still under active
> development and should not be expected to be stable until 1.0. Commands,
> flags, the MCP and HTTP surfaces, and the `content-source.yaml` schema may
> all change between releases, and breaking changes ship in 0.x minor
> versions rather than waiting for a major bump. Pin a release tag if you
> need reproducibility.

## Install

First time install is manual; subsequent updates run via `mk update`.
The repo is public, so both work anonymously — no login, token, or
PAT required. If `gh auth login` has been run, `mk update` and the
commands below reuse the cached token for a higher GitHub API rate
limit, but it's optional.

### From a release tarball (recommended)

Releases are published to [GitHub Releases](https://github.com/zegit-zoo/meerkat/releases).

```bash
# pick your platform
PLATFORM=darwin_arm64   # darwin_arm64 / darwin_amd64 / linux_amd64 / linux_arm64

# download + extract the latest release in one step
gh release download \
  --repo zegit-zoo/meerkat \
  -p "meerkat_*_${PLATFORM}.tar.gz" \
  --output - \
  | tar -xz -C ~/.local/bin meerkat

ln -sf meerkat ~/.local/bin/mk     # convenience short alias
meerkat version
```

No tag is pinned above — `gh release download` with no tag argument
fetches the latest release. If you hit GitHub's anonymous API rate
limit, `gh auth login` (or `export GH_TOKEN=...`) raises it; see
[docs/INSTALL.md](docs/INSTALL.md) for a curl/wget alternative and
signature verification.

If `~/.local/bin` isn't on your `$PATH`, add it (see [docs/INSTALL.md](docs/INSTALL.md)).

### From source

```bash
git clone https://github.com/zegit-zoo/meerkat.git
cd meerkat
make build              # → bin/meerkat (and bin/mk symlink)
make install            # → ~/.local/bin/{meerkat,mk}
```

### Updating

```bash
mk update --check       # newest GitHub release
mk update               # download + verified swap
```

## Use

> **The knowledge base ships empty.** The public repo intentionally embeds
> no content — `internal/kb/content/` and `internal/sources/etc/` hold only
> placeholders, so every example below returns nothing on a fresh build.
> Point meerkat at your own content repo by adding a `content-source.yaml`
> at the repo root, e.g.:
>
> ```yaml
> content:
>   type: local
>   path: ../your-kb-repo
> ```
>
> `type: local` needs no credentials of any kind — this is the default for
> most users, who build from their own knowledge-base directory on disk.
> (`type: git` and `type: submodule` also work — see
> [`content-source.example.yaml`](content-source.example.yaml) and
> [docs/design/content-sources.md](docs/design/content-sources.md) for the
> full schema.) For `type: git`, a private GitHub repo (`host: github`)
> automatically uses a cached `gh` CLI token if one is present; GitLab (or
> any other host) has no credential borrowing — use a full clone URL / SSH
> spec, or your normal git credential configuration, for private access.
> `make build` runs `make sync`, which reads this file and populates the
> embed dirs before compiling. To update content **without** rebuilding, see
> ["Serving content at runtime"](#serving-content-at-runtime) below.

```bash
# Knowledge base (offline, always available)
mk search "rate limiting"
mk search "circuit breaker" --limit 20
mk show concepts/Rate-Limiting
mk list --prefix systems/backend/
mk list --category policies --status placeholder
mk list --owner team-payments --json
mk list --type "BigQuery Table"                       # OKF's concept-kind field — see docs/OKF.md

# Servers
mk mcp serve                                          # stdio for OpenCode
mk http serve --port 4004                             # for OpenWebUI

# Ingestion (drives OpenCode sub-agents to populate placeholders)
mk ingest                                             # plan all stale tasks (JSONL)
mk ingest --source policies                           # plan one source
mk ingest --source policies --execute --max-parallel 4
mk ingest --page concepts/Rate-Limiting --execute
mk ingest sources                                     # show embedded source registry
mk ingest --batch-file batch.jsonl                    # write plan to file

# Operations
mk update --check                                     # newest GitHub release
mk version
```

Full per-command help: `mk <cmd> --help`.

## OpenCode integration

Add to `~/.config/opencode/opencode.json`:

```json
{
  "mcp": {
    "meerkat": {
      "type": "local",
      "command": ["mk", "mcp", "serve"],
      "enabled": true
    }
  }
}
```

Restart OpenCode. Three tools become available to the agent:

| Tool | Args | Returns |
|---|---|---|
| `mk_search` | `query`, `limit?` | `[{id, title, score, snippet, category, status}]` |
| `mk_show` | `id` | `{id, title, body, front, trust_tier, stale}` (parsed frontmatter, plus two OKF-derived advisory signals — see [docs/OKF.md](docs/OKF.md#trust-and-lifecycle)) |
| `mk_list` | `prefix?`, `category?`, `status?`, `owner?`, `type?` | `[{id, title, category, status, owner, type, source}]` |

## OpenWebUI integration

Run the HTTP server with a bearer token. Default bind is
loopback-only:

```bash
export MEERKAT_API_KEY=$(openssl rand -hex 32)
mk http serve --port 4004
```

Register `http://127.0.0.1:4004/openapi.json` as a Tool Server in OpenWebUI.
The same three tools are available as `POST /search`, `POST /show`,
`POST /list`. `/healthz` and `/openapi.json` are exempt from auth.

If OpenWebUI runs on another host, don't just add `--host 0.0.0.0` —
meerkat has no TLS of its own, so that puts the bearer token on the
wire in plaintext. See
[docs/INTEGRATION-OPENWEBUI.md](docs/INTEGRATION-OPENWEBUI.md#openwebui-on-another-host)
for the reverse-proxy pattern (recommended over direct exposure).

## Ingestion pipeline

Pages start as **placeholders** with frontmatter pointing at their upstream
source. An OpenCode (or Claude Code) sub-agent populates each one using the
prompt declared in the source's entry in your content repo's
`ingestion/sources.yaml`.

```
embedded sources.yaml
        │
        ▼
mk ingest (planner) ──► JSONL batch
                              │
                              ▼
mk ingest --execute (executor) ──► spawns one
                                    `opencode run --model openai/gpt-5.5-fast`
                                    per page, 5-min wall-clock cap
                                    │
                                    ▼
                              writes wiki/<page>.md, commits, pushes
                              the branch resolved from content-source.yaml
```

**Sources** are declared in your content repo's `ingestion/sources.yaml` —
see [docs/design/content-sources.md](docs/design/content-sources.md) for the
schema. Example registry (illustrative, not a real config):

| Category | Source |
|---|---|
| `adr/` | `your-org/architecture/architecture-decision-record` |
| `adr/` (security) | `your-org/security/security-decisions` |
| `threat-models/` | `your-org/architecture/threat-model` |
| `requirements/` | `your-org/requirements` (whole group) |
| `operations/{dev-portal,pipelines,base-images,runbooks}` | `your-org/operations/<repo>` |
| `systems/backend/` | `your-org/backend` (whole group) + service-catalog + incident enrichment |
| `systems/frontend/` | `your-org/frontend` (whole group, with subgroups) + same enrichment |
| `policies/` | PDF corpus in your extracted-docs directory |
| `concepts/` | Synthesised — cross-links from all other categories |

The Go binary ships **without LLM credentials**. The actual model calls happen
inside `opencode run` subprocess sessions, which inherit the user's OpenCode
config (model providers, MCP server connections, etc).

## Serving content at runtime

By default the wiki is embedded at build time (see ["Use"](#use) above), so
picking up new content means rebuilding. Four other mechanisms serve content
without a rebuild; `mk`/`meerkat` resolves one at startup, in this order —
highest priority first, each step consulted only if the one above is unset
(steps 1-2) or not found (steps 3-4):

1. `--kb-dir` (or `MEERKAT_KB_DIR`) — an explicit content-repo directory.
   Wins outright over everything below.
2. `--content-source` (or `MEERKAT_CONTENT_SOURCE`) — an explicit path to a
   `content-source.yaml`.
3. `content-source.yaml` in `<user config dir>/meerkat/` (`~/.config/meerkat/`
   on Linux, `~/Library/Application Support/meerkat/` on macOS).
4. `content-source.yaml` in the working directory (wherever `mk`/`meerkat`
   is invoked from — not a repo root).
5. The embedded build — the fallback when none of the above apply (the
   single-self-contained-binary property is unchanged when no directory or
   config is present).

Once a step is used, its `content.type` decides the outcome on its own —
including `type: none`, which resolves to the embedded fallback without
falling through to a lower step.

### `--kb-dir` / `MEERKAT_KB_DIR`

Points meerkat at a directory on disk instead of the embedded build — `mk
search`/`show`/`list` then serve that content directly, no rebuild required:

```bash
mk --kb-dir ./meerkat-kb search "rate limiting"
MEERKAT_KB_DIR=./meerkat-kb mk list
```

Precedence: `--kb-dir` flag, then `MEERKAT_KB_DIR` (step 1 above).

The directory uses the **content-repo layout** — the same layout
`content-source.yaml` describes and `mk ingest` writes into — not the
internal embed layout:

```
meerkat-kb/
├── wiki/                       # markdown pages
│   ├── index.md
│   └── concepts/
│       └── widgets.md
├── ingestion/
│   ├── sources.yaml            # source registry
│   └── prompts/
│       └── general.md          # per-source sub-agent prompts
└── templates/
    └── default.md               # page templates
```

Because this is the same layout `mk ingest --execute` commits into, pointing
`--kb-dir` at a working copy of your content repo means ingest output
becomes visible with no rebuild between ingesting a page and searching it.

A `--kb-dir` that doesn't exist is a hard error (exit 1). A directory that
exists but is missing `wiki/`, `ingestion/`, or `templates/` degrades to
empty for the missing piece — same as the public build's zero-content embed.

`--kb-dir`/`MEERKAT_KB_DIR` always use the default paths shown above, even
if a `content-source.yaml` elsewhere declares a custom `layout:` block — a
bare directory flag has nowhere to carry a layout override. A content repo
with a non-default layout looks empty through `--kb-dir`; point
`--content-source` (below) at a `type: local` config with the right
`layout:` instead.

### `content-source.yaml` at runtime

When `--kb-dir`/`MEERKAT_KB_DIR` is unset, meerkat looks for a
`content-source.yaml` (steps 2-4 above). An explicit `--content-source`/
`MEERKAT_CONTENT_SOURCE` path that doesn't exist is a hard error — same
reasoning as `--kb-dir`: the operator named it, so silently falling through
would be confusing. Only `content.type: none`, `local`, and `url` are valid
at runtime:

```bash
meerkat --content-source ./content-source.yaml list
MEERKAT_CONTENT_SOURCE=./content-source.yaml meerkat list
```

- **`none`** (or no file found at all) serves the embedded build.
- **`local`** resolves a relative `path` against **the config file's own
  directory** — not the working directory, and not a repo root. (This
  differs from the build-time resolver, which resolves it against the repo
  root `make sync` runs from.) An absolute `path` behaves the same either
  way. A resolved directory that doesn't exist is a hard error, same as
  `--kb-dir`.
- **`url`** fetches and caches an HTTPS archive — see below.
- **`git`** and **`submodule`** are build-time only (they need git and a
  working tree, which a shipped binary can't assume): naming one here fails
  with an explicit error rather than silently serving nothing. Run `make
  sync` to embed it at build time instead, or switch to `type: local`/
  `type: url` for a runtime-resolved source.

A `layout:` block in this file **is** honoured at runtime for `type: local`
and `type: url` sources — unlike `--kb-dir`, above.

### `type: url`

Fetches an HTTPS `.tar.gz` of the content-repo layout, verifies it, and
caches the extracted result:

```yaml
content:
  type: url
  url: https://example.com/kb/v1.2.3.tar.gz
  sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
```

`sha256` is **required**, not optional — compute it the way you'd verify any
other download (`shasum -a 256 kb.tar.gz` on macOS, `sha256sum kb.tar.gz` on
Linux). It's checked before anything is extracted: on a mismatch, nothing is
extracted and nothing is cached. That requirement is the feature, not an
inconvenience — it's what makes fetched content verifiable at all, and it
doubles as the on-disk cache key:

```
<user cache dir>/meerkat/content/url/<sha256>/
```

(`~/.cache/meerkat/...` on Linux, `~/Library/Caches/meerkat/...` on macOS —
`os.UserCacheDir()`, a different directory from the `<user config dir>` used
for discovery in step 3 above.) Content is immutable by digest: once a
digest is cached, any restart that resolves to it — the same
`content-source.yaml`, or a different one naming the same `sha256` — is a
cache hit and does **no network I/O at all**. To publish new content,
publish a new archive under a new digest and update `sha256` (and typically
the version in `url`) to match; nothing re-fetches on its own.

`type: url` is runtime-only: `make sync` does not fetch it, so — unlike
`local`/`git`/`submodule` — it cannot be embedded at build time.

See [content-source.example.yaml](content-source.example.yaml) for the full
schema (including `layout:`), and
[docs/SECURITY.md](docs/SECURITY.md#kb_commit-vs-kb_source-the-provenance-split)
for exactly what the digest does and does not guarantee.

### Provenance: `mk version`

`mk version` reports which content is actually being served via the
`kb_source` field:

| `kb_source` | Set by |
|---|---|
| `embedded` | No runtime content configured — the fallback. |
| `disk:<path>` | `--kb-dir`/`MEERKAT_KB_DIR`, or a `type: local` `content-source.yaml` source. Unverified — meerkat trusts the directory as-is. |
| `url:<url>@<digest12>` | A `type: url` `content-source.yaml` source — `<digest12>` is the first 12 hex characters of the verified `sha256` (e.g. `url:https://example.com/kb/v1.2.3.tar.gz@e3b0c44298fc`). |

`kb_commit` is unchanged by any of this — it always names the build-time
embedded content's commit, never a runtime directory's or archive's. See
[docs/SECURITY.md](docs/SECURITY.md) for what that split means for
provenance.

### OKF bundles

Any of the mechanisms above can point at an unmodified
[OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
(Open Knowledge Format) knowledge bundle instead of a meerkat-authored
content repo — `list`/`show`/`search` all work, no conversion step:

```yaml
content:
  type: local
  path: /path/to/parent-of-the-bundle   # the bundle's PARENT directory
  layout:
    wiki: name-of-the-bundle-directory  # the bundle itself, unmodified
```

A bundle's own root directory does not match the `wiki/`-rooted
content-repo layout meerkat expects, so `layout.wiki` has to name the
bundle's own directory as shown above — pointing it at `.` (the bundle
root itself, unrenamed) does not work. meerkat implements the OKF
**consumer** side only: it is an independent, third-party consumer, not
affiliated with or endorsed by OKF's authors (Google Cloud Platform).
See [docs/OKF.md](docs/OKF.md) for the full mapping from OKF frontmatter
to meerkat's, the trust-tier/staleness signals it surfaces, and what's
deliberately not implemented (cross-link resolution, the Attested
Computation family).

## How search works

Bleve full-text BM25 index, built in-memory at startup from the embedded
markdown. Title gets ×5 boost, ID gets ×3, body baseline. Reference
measurement from an internal deployment: cold-start ~150 ms on ~700 pages.
The public repo ships no content, so your own cold-start time depends on
the size of the content repo you point `content-source.yaml` at.

## Build / test / release

```bash
make build           # binary in bin/meerkat (+ bin/mk symlink)
make test            # go test -race ./...
make test-cover      # ... with coverage
make smoke           # end-to-end CLI sanity: version + list + search + show
make sync            # populate embed dirs from content-source.yaml (no-op, empty KB, if absent)
make install         # → ~/.local/bin/{meerkat,mk}

# Documentation
make docs            # regenerate docs/CLI.md from the cobra command tree
make docs-check      # CI gate: docs/CLI.md matches the current cobra tree

# Security (see docs/SECURITY.md)
make vuln            # govulncheck — known CVEs in our import graph
make gosec           # gosec — Go-specific weaknesses
make gitleaks        # gitleaks — committed secrets
make security        # all three at once

# Release helpers
make release-check     # validate .goreleaser.yaml
make release-snapshot  # local cross-platform build, no publish
```

`.github/workflows/ci.yml` runs four independent jobs — `lint`, `test`,
`vuln`, `gitleaks` — in parallel on every push to `main` and every pull
request; there's no job-to-job dependency. `.github/workflows/release.yml`
runs separately, triggered by a version tag: it re-runs the full gate (adding
`gosec`) on the tagged commit, then publishes via goreleaser only if that
passes.

Contributions go through a normal fork → branch → pull-request flow against
`main`; CI must pass before merge. There's no direct push to `main`.

Run the full local CI gate (`go vet` + `gofmt` + `go test -race` +
`docs-check`) before pushing — saves a round-trip:

```bash
make pre-push          # one-shot
make install-hooks     # installs .git/hooks/pre-push so it runs
                       # automatically on every `git push`
                       # (skip with `git push --no-verify`)
```

The `release` job uses goreleaser to cross-build, generate SBOMs
(via syft), sign the checksums file with cosign keyless (Fulcio +
Rekor), and publish a GitHub Release. See `docs/INSTALL.md` for the
verification flow on the consumer side.

Embedded content comes from whatever `content-source.yaml` points at (see
["The knowledge base ships empty"](#use) above and
[docs/design/content-sources.md](docs/design/content-sources.md)). To refresh
embedded content after the upstream source changes, just rebuild:

```bash
make build     # runs `make sync`, which re-resolves content-source.yaml
```

If you're using `type: submodule`, update the submodule first:

```bash
make kb-update   # git submodule update --remote --recursive
make build
```

## Repo layout

```
cmd/meerkat/main.go         entrypoint
internal/
  kb/         Page + Frontmatter, //go:embed all:content
  kbdir/      resolves --kb-dir/MEERKAT_KB_DIR, adapts it onto kb/sources
  search/     Bleve in-memory BM25 index
  sources/    embeds sources.yaml + prompts + templates
  ingest/     Plan(opts) + Run(ctx, tasks) — planner + executor
  mcp/        stdio MCP server (mk_search / mk_show / mk_list)
  http/       HTTP/OpenAPI server with bearer auth
  update/     mk update — gh token, GitHub Releases download, atomic swap
  cli/        cobra command tree
  clidocs/    docs/CLI.md generator (cobra tree -> single-file MD)
docs/
  CLI.md      auto-generated CLI reference (make docs)
  INSTALL.md  install + verify + troubleshooting
  SECURITY.md threat model + scanner suite + fix workflows
content-source.yaml   optional, not shipped; tells `make sync` (build) or
                      meerkat itself (runtime) where KB content lives
                      (local path / git repo / submodule / url archive)
```

## See also

- [zegit.dev/documentation/meerkat.html](https://zegit.dev/documentation/meerkat.html)
  — official meerkat documentation
- [zegit.dev/documentation/meerkat-cli.html](https://zegit.dev/documentation/meerkat-cli.html)
  — CLI reference and integration guides
- [docs/OKF.md](docs/OKF.md) — serving an [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
  (Open Knowledge Format) bundle unmodified, and what meerkat does with
  its frontmatter
- [docs/design/content-sources.md](docs/design/content-sources.md) — how
  `content-source.yaml` maps a content repo onto the embed dirs
- [docs/design/ingestion-pipeline.md](docs/design/ingestion-pipeline.md) —
  how `mk ingest` populates placeholder pages from that content repo
- Your own content repo holds the KB pages and `ingestion/sources.yaml` —
  meerkat only needs a `content-source.yaml` pointing at it
