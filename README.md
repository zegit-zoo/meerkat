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

On macOS with Homebrew, the `mk` shorthand above can collide with
`homebrew/core/mk` (the unrelated Plan 9 `mk` build tool) if it's
also installed — whichever one is first on `$PATH` wins, silently.
See [Homebrew `mk` collision](docs/INSTALL.md#homebrew-mk-collision)
for `$PATH` ordering / alias workarounds.

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

### Container image

Every `vX.Y.Z` tag also publishes a hardened, multi-arch (linux/amd64,
linux/arm64) OCI image to `ghcr.io/zegit-zoo/meerkat`, signed keylessly
with cosign and carrying SLSA provenance + a Syft SBOM. Image tags drop
the git tag's leading `v` (`v1.2.3` publishes `1.2.3`, `1.2`, and
`latest` — never `v1.2.3`):

```bash
docker pull ghcr.io/zegit-zoo/meerkat:1.2.3

docker run --rm --read-only --user 65532:65532 \
  ghcr.io/zegit-zoo/meerkat:1.2.3 \
  http serve --host 0.0.0.0 --api-key "$MEERKAT_API_KEY"
```

The image runs non-root (numeric UID/GID `65532`) on a distroless base
and needs no writable filesystem for the default embedded-content path —
`--read-only` above is not just permitted, it's the recommended way to
run it. See [docs/CONTAINER.md](docs/CONTAINER.md) for the full run
reference (read-only-fs flags, the cache-dir mount needed only for
`--content-source` `type: url`, and cosign verification).

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

# Multiple collections (when content-source.yaml declares any)
mk list --collections                                 # what's mounted
mk search "incident" --collection runbooks            # search one collection
mk show runbooks:incidents/paging                     # a collection-qualified page ID

# Servers
mk mcp serve                                          # stdio MCP for OpenCode
mk mcp serve-http --port 4005                         # hosted MCP (Streamable HTTP + OIDC)
mk http serve --port 4004                             # HTTP/OpenAPI for OpenWebUI

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
| `mk_save_memory` | `scope`, `title`, `content`, `key?`, `tags?`, `version?`, `replace?` | `{status, id, version, location, searchable}` — only when a collection declares a [`memory:`](#memory-mk_save_memory) store |

## Hosted MCP server (Streamable HTTP + OIDC)

`mk mcp serve` is one process per client, spawned by that client, trusted
because you started it. For **one meerkat serving many people**, run the
hosted transport instead:

```bash
mk mcp serve-http --port 4005          # http://127.0.0.1:4005/mcp
```

It exposes the same `mk_search` / `mk_show` / `mk_list` tools over the MCP
Streamable HTTP transport, with concurrent sessions, plus:

| endpoint | auth | what it is |
|---|---|---|
| `/mcp` | OIDC bearer | the MCP endpoint (POST / GET-SSE / DELETE) |
| `/.well-known/oauth-protected-resource` | none | RFC 9728 protected-resource metadata |
| `/livez` | none | liveness (process only) |
| `/readyz` | none | readiness — content resolution + per-collection index health |
| `/metrics` | none | Prometheus |

With **no `auth:` block configured** it is unauthenticated and serves every
mounted collection to any caller — the same posture as `mk mcp serve`, just
over HTTP. Bind loopback (the default) or put a gateway in front.

### Authentication and per-collection authorization

Add an `auth:` block to `content-source.yaml` (or pass a standalone policy
file with `--auth-config`):

```yaml
auth:
  resource: https://mcp.example.com/mcp        # RFC 9728 resource identifier
  providers:
    - issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
      audience: api://meerkat
      claims: { groups: groups, email: preferred_username, tenant: tid }
  rules:
    - name: sre
      groups: [sre, oncall]
      collections: [runbooks, architecture]
      capabilities: [read]
    - name: platform-admins
      groups: [platform-admins]
      collections: ["*"]
      capabilities: [admin]
```

OIDC discovery is generic — **Entra ID, Google Workspace and Okta are
configuration, not code**. Every request to `/mcp` must then carry a token
whose signature, issuer, audience and expiry verify; one that doesn't gets
`401` with a `WWW-Authenticate` header pointing at the metadata endpoint, so
a client can discover where to get a token with nothing configured
out-of-band.

**A collection a caller may not read is invisible, not denied.** It is
absent from their tool descriptions, from search results and listings, from
the "available: …" list in an error, and from `mk_show`'s ambiguity
resolution — indistinguishable from a collection this deployment never
mounted. A caller granted exactly one collection gets the plain,
single-collection UX. This matters because the alternative (a 403 per
operation) turns every one of those messages into an oracle for what exists
and what it's called.

Capabilities are `read`, `personal-write`, `team-write`, `global-write` and
`admin`. `read` decides visibility; the three write capabilities gate
[`mk_save_memory`](#memory-mk_save_memory), one per scope; `admin` implies
every capability, including ones a later meerkat adds — grant it sparingly.

Full schema: [content-source.example.yaml](content-source.example.yaml).
Design and threat reasoning: [docs/design/hosted-mcp.md](docs/design/hosted-mcp.md).

## Memory (`mk_save_memory`)

Give a collection a **memory store** and agents can save what they learn —
a decision and its reasoning, a convention, a correction you made — as a
Markdown page that is searchable by the very next `mk_search` call, with no
restart:

```yaml
collections:
  - name: team-notes
    type: local
    path: ../notes
    memory:
      type: local            # or: type: gcs, with bucket + prefix
      path: memory           # a SIBLING of wiki/, not inside it
      personal_visibility: private   # private (default) | collection
```

With no `memory:` block anywhere the tool is not registered at all, and
nothing else changes — which is what every configuration written before
this feature gets.

**Three scopes, three capabilities, two read audiences:**

| scope | needs | if you don't hold it | who can read it |
|---|---|---|---|
| `personal` | `personal-write` | refused (a personal memory has no reviewer) | **you alone** |
| `team` | `team-write` | **staged for review**, if you hold any other write capability | every reader of the collection |
| `global` | `global-write` | **staged for review**, likewise | every reader of the collection |

A staged memory lands under `<store>/_staging/<scope>/<namespace>/` with
`status: pending-review`. It is **not** searchable, showable or listable —
the store sits outside the served content tree, so a pending proposal cannot
become readable by being forgotten about. The tool's response says exactly
where it went and which capability would promote it. Promotion is `mv`
today; a review command is future work.

**You cannot save as somebody else.** A personal memory's namespace is
derived from your verified OIDC `sub` and `iss` and from nothing else —
there is no `namespace`, `subject`, `owner` or `author` argument, because
there is none to offer. Your `key` chooses *where inside your own space* a
memory goes; nothing chooses whose space that is. (On stdio, which has no
token and one user, personal memories land in a fixed `local` namespace.)

**Two agents cannot silently overwrite each other.** Every write carries an
optimistic-locking precondition — create-only by default, or conditioned on
the `version` a previous save returned. A local store compares a content
hash and writes temp-file + atomic rename; a GCS store uses
`ifGenerationMatch` preconditions the backend evaluates itself, so several
replicas can share one store. A lost race is a retryable `conflict` naming
the current version, never a silent overwrite:

```
conflict: the memory "memory/team/runbook" was created or changed by someone
else since you last read it, so this save was refused rather than overwriting
it. It is now at version "3f2a1c0b9d8e7f60". Read it with mk_show, merge what
you wanted to add, and save again with version="3f2a1c0b9d8e7f60".
```

Memories are ordinary pages under a reserved `memory/` prefix, so
`mk list --prefix memory/` and `mk search` find them like anything else —
subject to who may read them.

**`personal` means private to read, too.** A personal memory is readable
only by the principal who saved it, identified by the verified OIDC
`(iss, sub)` pair its namespace came from. To everybody else — including
holders of `read` on the same collection, including `admin` — it is not
refused, it is *absent*: missing from `mk_search`, from `mk_list`, from the
page counts in `mk_list_collections`, and from `mk_show`, which answers a
guessed ID exactly as it answers an ID nobody ever wrote. Filtering happens
inside the search query rather than over its results, so a hidden memory
never consumes a slot in your `limit` either.

Three things follow, and are worth knowing:

- **Ownership is `(iss, sub)` and nothing else.** Change team, email address
  or tenant and you keep your memories. Two identity providers that both
  mint `user-1` are two different people.
- **`team` and `global` are unchanged**: readable by every reader of the
  collection, exactly as before.
- **Locally it makes no difference.** `mk search`/`mk list`/`mk show` and
  `mk mcp serve` serve a single user, who owns the `local` namespace their
  own memories are written into.

An operator who deliberately wants the old, collection-wide behaviour asks
for it by name, per collection — `personal_visibility: collection` — and a
hosted server running OIDC logs a warning at startup saying so.

Design and threat reasoning: [docs/design/memory.md](docs/design/memory.md).

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
schema. Example registry for a fictional company — invented to show the
shapes a source can take, not a configuration to copy:

| Category | Source |
|---|---|
| `decisions/` | `your-org/engineering/decision-records` (one repo) |
| `handbook/` | `your-org/handbook` (whole group) |
| `systems/backend/` | `your-org/backend` (whole group) + service-catalog and past-incident enrichment |
| `systems/frontend/` | `your-org/frontend` (whole group, with subgroups) |
| `runbooks/{deploy,oncall,recovery}` | `your-org/operations/<repo>` (one source per repo) |
| `policies/` | PDF corpus in a local directory |
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

Steps 2-4 can also mount **several named collections** at once instead of a
single source — see [Multiple collections](#multiple-collections).

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
would be confusing. Only `content.type: none`, `local`, `url`, and `gcs` are
valid at runtime:

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
- **`gcs`** loads a Google Cloud Storage `.tar.gz` object or bucket prefix —
  see below.
- **`git`** and **`submodule`** are build-time only (they need git and a
  working tree, which a shipped binary can't assume): naming one here fails
  with an explicit error rather than silently serving nothing. Run `make
  sync` to embed it at build time instead, or switch to `type: local`/
  `type: url`/`type: gcs` for a runtime-resolved source.

A `layout:` block in this file **is** honoured at runtime for `type: local`,
`type: url` and `type: gcs` sources — unlike `--kb-dir`, above.

The same file can instead declare **several named collections**, mounted at
once — see [Multiple collections](#multiple-collections) below.

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

### `type: gcs`

Loads content from a Google Cloud Storage bucket, in one of two modes.
Runtime-only, like `type: url`.

```yaml
# (a) bundle mode — one .tar.gz object
content:
  type: gcs
  bucket: my-org-knowledge          # bucket NAME, not a gs:// URL
  object: bundles/kb-v1.2.3.tar.gz
  # generation: 1748112233445566    # optional: pin one exact generation

# (b) prefix mode — an object prefix served as a directory tree
content:
  type: gcs
  bucket: my-org-knowledge
  prefix: kb/live/                  # stripped: kb/live/wiki/x.md -> wiki/x.md
```

**Credentials: Application Default Credentials, and nothing else.** Workload
Identity Federation, a GKE/Cloud Run/GCE service account, an impersonated
principal, or `gcloud auth application-default login` for a developer. There
is deliberately **no key-file field in the schema** — meerkat cannot be asked
to load a static service-account key. Grant the principal
`storage.objects.get`, plus `storage.objects.list` for prefix mode.

**Caching is by object generation.** GCS assigns a new generation on every
write, so `(bucket, object, generation)` names immutable bytes the way a
`sha256` does for `type: url` — which is why `sha256` is *optional* here
(and still verified before extraction if you set it):

```
<user cache dir>/meerkat/content/gcs/<hash(bucket,object)>/<generation>/
<user cache dir>/meerkat/content/gcs/<hash(bucket,prefix)>/<listing-fingerprint>/
```

In prefix mode the key is a fingerprint over every listed object's
`(name, generation)`, so any add, overwrite or delete under the prefix
invalidates it. Either way a restart on unchanged content is a cache hit;
overwriting the object publishes new content on the next start, with the
previous generation's cache entry left intact for a rollback.

Objects are fetched with a conditional read (an explicit generation **and**
`ifGenerationMatch`), so the bytes in a cache entry named `<generation>`
cannot be some other generation's. Setting `generation:` explicitly pins the
deployment — the current generation is then never even looked up, so a later
overwrite cannot change what this binary serves.

#### `refresh:` — follow the bucket without a restart

By default a GCS source is resolved **once**, at startup: publishing a new
generation needs a restart or a rollout. Add a `refresh:` block and a running
`mk mcp serve-http` follows the bucket instead.

```yaml
collections:
  - name: handbook
    type: gcs
    bucket: my-org-knowledge
    prefix: handbook/live/
    refresh:
      interval: 60s               # required, >= 5s, unit mandatory
      jitter: 10s                 # optional, < interval
      failure_policy: serve-last-good   # or: unready
    memory:
      type: gcs
      bucket: my-org-knowledge
      prefix: handbook/memory/
      refresh:
        interval: 15s             # replicas converge on each other's writes
```

Each interval, meerkat reads **metadata only** — the object's generation, or
the fingerprint over the prefix listing. Unchanged (the usual answer) costs
one call and nothing else. Changed, and it re-resolves through the same
hardened, generation-preconditioned path startup uses, rebuilds the index off
the request path, and swaps the whole snapshot in atomically. Queries are
served the entire time: in-flight ones finish against the generation they
started on, new ones get the new generation, and nobody ever sees a mixture.

If a refresh fails, the **last known-good content keeps serving** and the
collection is marked degraded — visible in `/readyz`'s counts and in
`meerkat_refresh_degraded`. `failure_policy: unready` additionally fails the
readiness probe, for a collection where stale content is a correctness
problem rather than an inconvenience. The detail behind it (which
generation is applied, when the last cycle succeeded, what failed) is on the
authenticated discovery surfaces — `mk_list_collections` and
`GET /collections` — not on the unauthenticated probes.

`refresh:` under a `memory:` block is what makes **several replicas sharing
one GCS memory store converge**: without it, a memory saved through one
replica stays invisible to the others until they restart.

`SIGHUP` runs every configured refresh immediately, through the same code
path. There is deliberately no HTTP reload endpoint.

`generation:` and `refresh:` are **mutually exclusive** and refused together
at load time: pinning means "serve exactly these bytes until the config
changes", and a file that also asks to follow the object has two
contradictory readings. Pick the posture you want:

| | `generation:` (pinned) | `refresh:` |
| --- | --- | --- |
| serves | exactly that generation, forever | whatever the bucket holds now |
| to change it | edit config, redeploy | publish to the bucket |
| reproducible | yes, indefinitely | eventually consistent within one interval |

Details, including the snapshot-swap and degradation model:
[docs/design/hot-reload.md](docs/design/hot-reload.md).

### Multiple collections

Instead of a single `content:` block, a `content-source.yaml` can declare a
list of **named collections** — separate knowledge bases mounted at once,
with backends that need not match:

```yaml
collections:
  - name: runbooks
    type: local
    path: ../runbooks-kb

  - name: architecture
    type: gcs
    bucket: my-org-knowledge
    object: bundles/architecture-v3.tar.gz

  - name: team-notes
    type: gcs
    bucket: my-org-knowledge
    prefix: notes/live/

  - name: vendor-docs
    type: url
    url: https://example.com/kb/vendor-v2.tar.gz
    sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
    layout:
      wiki: docs
```

Each entry takes exactly the keys `content:` takes (including its own
`layout:`), plus a `name`. Names must match `[A-Za-z0-9][A-Za-z0-9_-]*` — no
colon, since a name is also the `<collection>:` prefix of a qualified page
ID. `content:` and `collections:` are mutually exclusive.

**Order matters.** It is the order collections are searched, listed, and
disambiguated in.

```bash
mk list --collections                      # what's mounted
mk list --collections --json               # name, type, provenance, page count

mk search "incident"                       # every collection, merged by score
mk search "incident" --collection runbooks # just one

mk list                                    # every collection, IDs qualified
mk list --collection architecture          # just one

mk show runbooks:incidents/paging          # a page ID qualified by collection
mk show incidents/paging --collection runbooks   # equivalent
mk show incidents/paging                   # tried in order (see below)
```

**Routing rules**, with several collections mounted:

| | `--collection`/`collection` given | omitted |
|---|---|---|
| search | that collection | all of them, merged by score, truncated to `--limit` |
| list | that collection | all of them, in configuration order |
| show | that collection | all in order — **one** match wins; **several** is an error |

`mk show <bare-id>` for an ID that exists in more than one collection
**fails**, listing the qualified IDs to choose from. It does not silently
return whichever collection happens to be configured first: the answer to
"show me this page" should never depend on file ordering the caller can't
see. (HTTP returns `409 Conflict` for this; MCP returns a tool-level error
the model can retry with.)

**Page IDs are never rewritten.** `--json` output, the MCP tools and the
HTTP endpoints all report the page's own unqualified `id` alongside a
separate `collection` field, so anything that round-trips an ID keeps
working. Only human-readable CLI output prints the `<collection>:<page-id>`
form, and only when more than one collection is mounted.

**MCP and HTTP.** `mk_search`/`mk_show`/`mk_list` take an optional
`collection` argument, and their descriptions name the mounted collections
so a client discovers the set from the tool list it already fetches.
`POST /search`, `/show` and `/list` take an optional `"collection"` field,
and `GET /collections` (auth-gated) enumerates what's mounted.

**A single collection behaves exactly as before.** Every configuration that
predates collections — including a plain `content:` block, `--kb-dir`, and
the embedded fallback — resolves to one collection named `default`, with
unqualified IDs everywhere and nothing to disambiguate.

**Not yet collection-aware:** `mk ingest`, the ingestion source registry,
and shell completion read the **first** configured collection. See
[docs/design/multi-collection.md](docs/design/multi-collection.md).

**Restricting who sees which collection** is a hosted-MCP feature — see
[Hosted MCP server](#hosted-mcp-server-streamable-http--oidc) above. The
CLI and `mk http serve` grant every mounted collection to whoever can run
the binary or present the static token.

### Update contract (`update:`)

A collection can also declare how knowledge flows back *into* it, so an
agent that learned something has a sanctioned path instead of a guess:

```yaml
collections:
  - name: handbook
    type: gcs                    # served from a mirror…
    bucket: my-org-knowledge
    prefix: handbook/live/
    description: Engineering handbook — conventions, onboarding, how we work.
    update:                      # …but maintained in git, so contributions
      method: merge-request      # go there: direct | merge-request | none
      repo: https://github.com/example-org/handbook.git
      host: github               # github | gitlab | other — which CLI to use
      branch: main
      path: wiki                 # where pages live in the CONTRIBUTION repo
      instructions: |
        Fork example-org/handbook and open the PR from your fork.
        One page per pull request.
```

The contract is **declared, never inferred** from the source type: a
serving mirror and a contribution repo are different addresses, and only
you know which is which. The one rule meerkat enforces is that
`method: direct` needs a backend a write can land in (a local directory or
a GCS prefix) — declaring it on a `type: url` archive, a `gcs` bundle or
the embedded build fails at **startup**, not at the first lost write.

What a caller is *told* depends on what that caller may do. Without
`global-write` on a `direct` collection they are pointed at the staging
path ([`mk_save_memory`](#memory-mk_save_memory)) or told there is none —
never at a write they would be refused for. Capabilities only ever narrow
a declared contract: an admin on a `merge-request` collection still opens
a merge request.

Not yet surfaced through the tools — that lands with collection
discovery. Design and rendering rules:
[docs/design/update-contract.md](docs/design/update-contract.md).

### Provenance: `mk version`

`mk version` reports which content is actually being served via the
`kb_source` field:

| `kb_source` | Set by |
|---|---|
| `embedded` | No runtime content configured — the fallback. |
| `disk:<path>` | `--kb-dir`/`MEERKAT_KB_DIR`, or a `type: local` `content-source.yaml` source. Unverified — meerkat trusts the directory as-is. |
| `url:<url>@<digest12>` | A `type: url` `content-source.yaml` source — `<digest12>` is the first 12 hex characters of the verified `sha256` (e.g. `url:https://example.com/kb/v1.2.3.tar.gz@e3b0c44298fc`). |
| `gcs://<bucket>/<object>@<gen>` | A `type: gcs` bundle source — `<gen>` is the object generation actually fetched. |
| `gcs://<bucket>/<prefix>*@<fp>` | A `type: gcs` prefix source — `<fp>` fingerprints the listing's `(name, generation)` pairs. |
| `collections:<n>` | Several collections are mounted; each one's own provenance is reported in the `collections` array (see below). |

Like `url:`, the token after `@` on a `gcs:` line is a *checked* property of
what's being served — the conditional read cannot return another generation
— not a label, unlike `disk:`.

With several collections mounted, `mk version --json` also carries a
`collections` array (`name`, `type`, `source`) in configuration order, and
the plain-text output itemises them, so a multi-collection deployment
reports everything it serves:

```json
{
  "kb_source": "collections:2",
  "collections": [
    {"name": "runbooks", "type": "local", "source": "disk:/srv/runbooks-kb"},
    {"name": "architecture", "type": "gcs", "source": "gcs://my-org-knowledge/bundles/architecture-v3.tar.gz@1748112233445566"}
  ]
}
```

A single-collection deployment reports one entry named `default`, with
`kb_source` unchanged from what it always was.

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

### Load speed with content from disk

The figure above is for content embedded at build time. If you take the
pristine binary and point it at your own knowledge base (see ["Serving
content at runtime"](#serving-content-at-runtime)), the index is built from
disk at startup instead — so the cost lands differently depending on how
meerkat is run.

Measured against [meerkat-bim](https://github.com/JonasLundin/meerkat-bim),
an OKF bundle of 67 concepts / 0.72 MB of markdown (averaging ~10 KB per
page), served with `--kb-dir` on an Apple M2 Pro (10 cores, macOS 26.6,
go1.26.5) — medians over 20 runs, warm filesystem cache:

| Invocation | Cost | When it is paid |
|---|---|---|
| `mk search` | ~160 ms | **every invocation** — the process exits and the index dies with it |
| `mk http serve`, `mk mcp serve`, `mk mcp serve-http` | ~160 ms once, then ~1 ms per query | at startup, before the port accepts traffic |
| `mk show`, `mk list` | ~10–20 ms | never — neither builds the search index |

A ~160 ms cold `mk search` breaks down as ~11 ms process start, ~10 ms
reading the markdown tree, and ~140 ms building the index. Reading content
off disk is close to free; the index build is essentially the whole cost, and
it tracks the **volume** of markdown rather than the number of files — around
0.2–0.3 s per MB:

| Concepts | Markdown | Server startup | Warm query (median) |
|---|---|---|---|
| 67 | 0.72 MB | 0.16 s | 1.0 ms |
| 134 | 1.42 MB | 0.33 s | 1.2 ms |
| 268 | 2.84 MB | 0.66 s | 1.3 ms |
| 536 | 5.68 MB | 1.38 s | 1.4 ms |
| 1072 | 11.36 MB | 3.01 s | 1.6 ms |

Two caveats on that table. Rows past the first are the same bundle
duplicated, so vocabulary repeats and a genuinely distinct corpus of that
size will index somewhat slower. And page size matters more than page count:
67 pages averaging 10 KB cost more to index than several hundred short ones,
so compare against your own markdown volume rather than your page count.

The practical consequence is that repeated querying belongs in a server, not
a shell loop. `mk http serve` pays the index build once and then answers in
about a millisecond; `mk search` in a loop pays it every time. Because
`search.New()` runs before the listener binds, a server that has started
answering `/healthz` has a fully built index — there is no window where it
serves queries against a partial one.

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
  contentsource/  content-source.yaml: build-time sync + runtime resolution
                  (local / git / submodule / url / gcs), collections
  collections/    named collections + search/show/list routing across them
  search/     Bleve in-memory BM25 index
  sources/    embeds sources.yaml + prompts + templates
  ingest/     Plan(opts) + Run(ctx, tasks) — planner + executor
  mcp/        MCP server (mk_search / mk_show / mk_list / mk_save_memory) —
              stdio and the hosted Streamable HTTP transport, probes,
              metrics, access log
  memory/     writable memory stores (local dir / GCS prefix) with
              optimistic locking, identity-derived namespaces, staging
  authn/      OIDC discovery/JWKS verification + the bearer gate (RFC 9728)
  authz/      capability model, access policy, per-collection grants
  http/       HTTP/OpenAPI server with bearer auth
  update/     mk update — gh token, GitHub Releases download, atomic swap
  cli/        cobra command tree
  clidocs/    docs/CLI.md generator (cobra tree -> single-file MD)
docs/
  CLI.md        auto-generated CLI reference (make docs)
  INSTALL.md    install + verify + troubleshooting
  CONTAINER.md  running the OCI image (read-only fs, cache-dir mount, verify)
  SECURITY.md   threat model + scanner suite + fix workflows
content-source.yaml   optional, not shipped; tells `make sync` (build) or
                      meerkat itself (runtime) where KB content lives
                      (local path / git repo / submodule / url archive /
                      GCS object or prefix), as one source or several
                      named collections, plus the optional auth: policy,
                      per-collection memory: stores and update: contracts
```

## See also

- [zegit.dev/documentation/meerkat.html](https://zegit.dev/documentation/meerkat.html)
  — official meerkat documentation
- [zegit.dev/documentation/meerkat-cli.html](https://zegit.dev/documentation/meerkat-cli.html)
  — CLI reference and integration guides
- [docs/CONTAINER.md](docs/CONTAINER.md) — running the OCI image (read-only
  root filesystem, the cache-dir mount, cosign verification)
- [docs/OKF.md](docs/OKF.md) — serving an [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
  (Open Knowledge Format) bundle unmodified, and what meerkat does with
  its frontmatter
- [docs/design/content-sources.md](docs/design/content-sources.md) — how
  `content-source.yaml` maps a content repo onto the embed dirs
- [docs/design/multi-collection.md](docs/design/multi-collection.md) —
  mounting several named collections at once, the routing/disambiguation
  rules, and the GCS backend's generation-keyed caching
- [docs/design/hot-reload.md](docs/design/hot-reload.md) — `refresh:`:
  the metadata-probe/atomic-snapshot-swap model that lets a running
  server pick up a new GCS generation and lets replicas converge on a
  shared memory store, why a pinned `generation:` refuses it, and what a
  failed refresh leaves serving
- [docs/design/hosted-mcp.md](docs/design/hosted-mcp.md) — the hosted
  Streamable HTTP MCP server: OIDC, the capability model, and why an
  unauthorized collection is made invisible rather than denied
- [docs/design/memory.md](docs/design/memory.md) — `mk_save_memory`: why
  the personal namespace is structurally unspoofable, why a personal
  memory is private to READ as well as to write (and why that filter has
  to live inside the search query), the scope→capability table, the
  optimistic-locking scheme, and the staging shape
- [docs/design/update-contract.md](docs/design/update-contract.md) — the
  per-collection `update:` contract: why it is declared rather than
  inferred, and how the path a caller is shown is narrowed to what that
  caller can actually do
- [docs/design/ingestion-pipeline.md](docs/design/ingestion-pipeline.md) —
  how `mk ingest` populates placeholder pages from that content repo
- [docs/design/index-filtering.md](docs/design/index-filtering.md) — an
  assessment (not yet implemented) of index-time frontmatter filtering
  for very large knowledge bases, with measurements
- Your own content repo holds the KB pages and `ingestion/sources.yaml` —
  meerkat only needs a `content-source.yaml` pointing at it
