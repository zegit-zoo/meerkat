# meerkat — the vigilant guard and informer

Part of the [zegit](https://zegit.dev/) platform — meerkat *knows*.
Full documentation: [zegit.dev/documentation/meerkat.html](https://zegit.dev/documentation/meerkat.html).

Single static binary that serves a knowledge base over:

- **CLI** — `mk search`, `mk show`, `mk list`, `mk lint`, `mk ingest`
- **MCP** server (Model Context Protocol) — for agent harnesses / OpenCode / Claude Desktop
- **HTTP/OpenAPI** server with bearer-token auth — for OpenWebUI

**The binary carries no content of its own.** It **loads a knowledge base at
runtime** — a directory on disk, a verified HTTPS archive, a GCS or S3
bucket, or several of those mounted side by side as named collections —
from a `--kb-dir` flag or a `content-source.yaml`. Nothing is fetched until
you point it somewhere, and search/show/list then answer from the resolved
content with no service to call. See ["Loading content"](#loading-content).

A build from source can instead bake content into the binary for a
self-contained offline artefact; that is the optional secondary path, and
it is the only way to use a git repo or a submodule as the source — see
["Embedding content at build time"](#optional-embedding-content-at-build-time).

> **Early development — pre-1.0.** This project is still under active
> development and should not be expected to be stable until 1.0. Commands,
> flags, the MCP and HTTP surfaces, and the `content-source.yaml` schema may
> all change between releases, and breaking changes ship in 0.x minor
> versions rather than waiting for a major bump. Pin a release tag if you
> need reproducibility.

## Install

On macOS and Linuxbrew, install from the tap. Everywhere else, grab a
release tarball; subsequent updates then run via `mk update`. The repo
is public, so all of it works anonymously — no login, token, or PAT
required. If `gh auth login` has been run, `mk update` and the
commands below reuse the cached token for a higher GitHub API rate
limit, but it's optional.

### Homebrew (macOS / Linux) — recommended where `brew` is available

```bash
brew install zegit-zoo/tap/meerkat
meerkat version

brew update && brew upgrade meerkat   # how a brew install updates
```

Under Homebrew's tap trust (6.0 and later) the full name above trusts only this
formula, so no `brew trust` step is needed; if you `brew tap
zegit-zoo/tap` first and install by bare name, run
`brew trust --formula zegit-zoo/tap/meerkat` before `brew install
meerkat`. The formula installs `meerkat`, the `mk` shorthand, and
bash/zsh/fish completions, and pins the sha256 of the release tarballs
linked below from the cosign-verified checksums file, re-verified on
every tap bump, so `brew install` inherits the release's signing
guarantees rather than replacing them.

Two consequences worth knowing up front:

- **`mk update` is disabled for Homebrew installs.** The binary lives
  in the Cellar and belongs to `brew`; an in-place swap would be
  undone by the next `brew upgrade`. `mk update` detects this and
  refuses with a pointer to `brew update && brew upgrade meerkat`. `mk update
  --check` still works — it only reads.
- **The formula declares `conflicts_with "mk"`**, the unrelated Plan 9
  `mk` build tool in homebrew/core. `brew` refuses to install both
  rather than letting them fight over `$PATH`; see [Homebrew `mk`
  collision](docs/INSTALL.md#homebrew-mk-collision) if you need both
  tools on one machine.

### From a release tarball (any platform)

Releases are published to [GitHub Releases](https://github.com/zegit-zoo/meerkat/releases).

```bash
PLATFORM=darwin_arm64   # darwin_arm64 / darwin_amd64 / linux_amd64 / linux_arm64

gh release download --repo zegit-zoo/meerkat \
  -p "meerkat_*_${PLATFORM}.tar.gz" --output - \
  | tar -xz -C ~/.local/bin meerkat      # no tag argument = latest release
ln -sf meerkat ~/.local/bin/mk           # convenience short alias
meerkat version
```

Windows ships a `.zip` instead. If you hit GitHub's anonymous API rate
limit, `gh auth login` (or `export GH_TOKEN=...`) raises it; see
[docs/INSTALL.md](docs/INSTALL.md) for Windows, a curl/wget alternative and
signature verification.

A tarball install is outside `brew`'s view, so the `conflicts_with`
guard above does not apply: if `homebrew/core/mk` is also installed,
whichever `mk` is first on `$PATH` wins, silently. See [Homebrew `mk`
collision](docs/INSTALL.md#homebrew-mk-collision). If `~/.local/bin`
isn't on your `$PATH`, add it.

### From source

```bash
git clone https://github.com/zegit-zoo/meerkat.git
cd meerkat
make build              # → bin/meerkat (and bin/mk symlink)
make install            # → ~/.local/bin/{meerkat,mk}
```

### Updating

```bash
mk update --check       # newest GitHub release (works everywhere)
mk update               # download + verified swap; refuses on a brew install
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

The image runs non-root (numeric UID/GID `65532`) on a distroless base, and
is built with no content source — so like every other published artefact it
needs a runtime knowledge base (bind-mount a directory and pass `--kb-dir`,
or mount a `content-source.yaml`). `--read-only` is not just permitted,
it's the recommended way to run it: a `type: local` source writes nothing at
all, and `type: url`/`gcs`/`s3` need only `/home/nonroot/.cache` to be
writable. See [docs/CONTAINER.md](docs/CONTAINER.md) for the full run
reference (read-only-fs flags, the content-cache mount, and cosign
verification).

## Use

A fresh install serves nothing until you point it at a knowledge base. The
shortest way is a directory on disk in the
[content-repo layout](#--kb-dir--meerkat_kb_dir) — no credentials, no config
file, nothing fetched. For anything you'd rather not repeat every invocation
— an HTTPS archive, a GCS or S3 bucket, several named collections, auth,
telemetry — write a `content-source.yaml` where meerkat discovers it:

```bash
mk --kb-dir ./meerkat-kb search "rate limiting"    # or: export MEERKAT_KB_DIR=…

CFG=~/.config/meerkat                                      # Linux
[ "$(uname)" = Darwin ] && CFG="$HOME/Library/Application Support/meerkat"
mkdir -p "$CFG"
printf 'content:\n  type: local\n  path: /path/to/your-kb-repo\n' \
  > "$CFG/content-source.yaml"
mk list                                            # now serves that directory
```

That config directory is the OS's own, not `~/.config` everywhere — hence the
`uname` guard. meerkat then resolves **one** source per invocation, highest
priority first: `--kb-dir`/`MEERKAT_KB_DIR`,
`--content-source`/`MEERKAT_CONTENT_SOURCE`, `<user config dir>/meerkat/`,
`./content-source.yaml`, then the binary's own embedded content — empty in
every published release. `mk version` always reports which one won, as
`kb_source`. Every backend and the rules in full:
["Loading content"](#loading-content).

```bash
# Knowledge base (answered locally, no service to call)
mk search "rate limiting"
mk search "circuit breaker" --limit 20
mk show concepts/Rate-Limiting
mk list --prefix systems/backend/
mk list --category policies --status placeholder
mk list --owner team-payments --json
mk list --type "BigQuery Table"                       # OKF's concept-kind field — see docs/OKF.md
mk lint                                               # dangling related:/pointer targets; exit 1 if any

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
mk ingest sources                                     # show the source registry in view
mk ingest --batch-file batch.jsonl                    # write plan to file

# Operations
mk update --check                                     # newest GitHub release
mk version                                            # incl. kb_source: what is being served
```

Full per-command help: `mk <cmd> --help`; generated reference in
[docs/CLI.md](docs/CLI.md).

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

Restart OpenCode. Five tools become available to the agent, plus
`mk_save_memory` when a collection declares a store:

| Tool | Args | Returns |
|---|---|---|
| `mk_search` | `query`, `limit?`, `collection?`, `bundle?`, `session_id?` | `[{id, title, score, snippet, category, status}]` |
| `mk_show` | `id`, `collection?`, `session_id?` | `{id, title, body, front, trust_tier, stale}` (parsed frontmatter, plus two OKF-derived advisory signals — see [docs/OKF.md](docs/OKF.md#trust-and-lifecycle)) |
| `mk_list` | `prefix?`, `category?`, `status?`, `owner?`, `type?`, `collection?` | `[{id, title, category, status, owner, type, source}]` |
| `mk_list_collections` | — | `[{name, type, source, pages, capabilities, …}]` — what this caller may read (see [Multiple collections](#multiple-collections)) |
| `mk_report_outcome` | `outcome`, `initial_query?`, `pages?`, `attempted?`, `quality?`, `fallback?`, `session_id?` | `{recorded, logged, intake_id?}` — see [Reporting outcomes](#reporting-outcomes-mk_report_outcome) |
| `mk_save_memory` | `scope`, `title`, `content`, `key?`, `tags?`, `version?`, `replace?` | `{status, id, version, location, searchable}` — only when a collection declares a [`memory:`](#memory-mk_save_memory) store |

## Hosted MCP server (Streamable HTTP + OIDC)

`mk mcp serve` is one process per client, spawned by that client, trusted
because you started it. For **one meerkat serving many people**, run the
hosted transport instead:

```bash
mk mcp serve-http --port 4005          # http://127.0.0.1:4005/mcp
```

It exposes the same tools as `mk mcp serve` over the MCP Streamable HTTP
transport, with concurrent sessions, plus:

| endpoint | auth | what it is |
|---|---|---|
| `/mcp` | OIDC bearer | the MCP endpoint (POST / GET-SSE / DELETE) |
| `/.well-known/oauth-protected-resource` | none | RFC 9728 protected-resource metadata |
| `/livez` | none | liveness (process only) |
| `/readyz` | none | readiness — content resolution + per-collection index health |
| `/metrics` | none | Prometheus |

With **no `auth:` block configured** it is unauthenticated and serves every
mounted collection to any caller — the same posture as `mk mcp serve`, just
over HTTP. Bind loopback (the default) or put a gateway in front. With one
configured, every collection needs a token unless it is
[explicitly published](#publishing-a-collection-to-unauthenticated-callers).

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

#### Publishing a collection to unauthenticated callers

Selected collections can be served to callers with **no token at all** —
a handbook, a published policy set — while everything else on the same
endpoint keeps requiring one. Add an `anonymous: true` rule:

```yaml
  rules:
    - name: public-handbook
      anonymous: true
      collections: [handbook, published-policies]
      capabilities: [read]        # anonymous rules are read-only
```

- **Explicit only.** With no such rule, a token-less request gets the
  same `401` it always did. A rule with *no* selector still means "every
  **authenticated** caller", so existing policies publish nothing.
- **Read-only.** Write capabilities on an anonymous rule fail the process
  at load. An anonymous caller is offered no write tool and owns no
  personal memories.
- **Everything else stays invisible**, not denied — a guessed page ID, a
  `<collection>:<id>` qualification and an ambiguity count all answer as
  if the collection were never mounted.
- **Authenticated callers get it too**, unioned with their own rules, so
  a published collection never needs restating.
- **A bad token is still `401`.** Expired, forged or malformed tokens are
  never silently downgraded to anonymous access.
- Cannot be combined with `allow_unauthenticated`, which publishes
  *everything*; the combination is refused at load.

A published collection is an **intentional disclosure surface**: its
pages, IDs, frontmatter and its `update:` contract — including a
`merge-request` repo URL — are readable by anyone who can reach the
endpoint. See [docs/SECURITY.md](docs/SECURITY.md#published-collections-are-an-intentional-disclosure-surface).

Full schema: [content-source.example.yaml](content-source.example.yaml).
Design and threat reasoning: [docs/design/hosted-mcp.md](docs/design/hosted-mcp.md).

### Observability (`observability:`) — traces, OTLP, domain metrics

Out of the box the hosted server gives you `/metrics` (Prometheus) and
structured JSON logs on stderr, and **that is all it gives you unless you
ask for more**. With no `observability:` block and no `OTEL_*` variable, no
OpenTelemetry SDK is constructed at all: no spans, no exporter, no
goroutine, no socket.

Ask for more, and one `mk_search` becomes one trace — the HTTP request, the
OIDC verification, the authorization decision, the tool call, the search,
the index access, and any GCS or memory work underneath it:

```yaml
observability:
  service_name: meerkat
  environment: production

  traces:
    enabled: true
    sample_ratio: 0.10        # head sampling for traces THIS process starts

  logs:
    include_trace_context: true   # trace_id/span_id on the access + auth logs

  metrics:
    prometheus: true          # /metrics, unchanged, plus bounded domain series
    otlp: false

  otlp:
    endpoint: otel-collector.observability.svc:4317
    protocol: grpc            # grpc | http/protobuf
    insecure: false
    # Credentials come from the environment. This names the VARIABLE;
    # the value never appears in this file.
    headers_env: OTEL_EXPORTER_OTLP_HEADERS
```

**Precedence:** explicit configuration beats `OTEL_*` environment beats
default, per field — so a file that sets only `traces.enabled: true` still
takes its endpoint from `OTEL_EXPORTER_OTLP_ENDPOINT`. Two deliberate
inversions: `OTEL_SDK_DISABLED=true` beats an explicit `enabled: true` (an
off switch a file can override is not an off switch), and
`OTEL_EXPORTER_OTLP_INSECURE` does not apply once the file has written an
`otlp:` block of its own. With no config file at all,
`MEERKAT_TRACES_ENABLED=true` plus the standard `OTEL_*` variables is the
container-friendly way in.

**What a span may say.** Counts, durations, booleans, outcomes from a closed
set, the matched route pattern, and a collection's configuration *ordinal*.
Never a query, a page ID, page or memory content, a collection name, a
bucket or object name, a token, a claim, a subject, a session ID or a
request path. The access log still carries `sub`/`issuer`/`tenant` for
audit — it stays on your stderr; a span goes to a collector, so it is held
to the stricter rule. A test walks every recorded span and metric label
asserting all of that.

**Failure behaviour.** A collector that is down never affects a search, a
memory write, `/readyz`, or how long a pod takes to terminate. The span
queue is bounded and drops rather than growing
(`meerkat_otel_spans_dropped_total`), export failures are counted
(`meerkat_otel_export_failures_total`) and logged once a minute, and the
shutdown flush is bounded by `limits.shutdown_timeout`.

Design, the full span/attribute taxonomy and the precedence table:
[docs/design/observability.md](docs/design/observability.md).

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

```text
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
The search/show/list tools are available as `POST /search`, `POST /show`,
`POST /list`, and `GET /collections` enumerates what's mounted. `/healthz`
and `/openapi.json` are exempt from auth; everything else needs the bearer
token, and the server refuses to start without one.

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

```text
sources.yaml (from the resolved content source)
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

## Loading content

This is the main path: an installed meerkat carries no content and is told
at startup where its knowledge base lives. Four mechanisms can say so, and
`mk`/`meerkat` consults them in this order — highest priority first, each
step reached only if the one above is unset (steps 1-2) or not found
(steps 3-4):

1. `--kb-dir` (or `MEERKAT_KB_DIR`) — an explicit content-repo directory.
   Wins outright over everything below.
2. `--content-source` (or `MEERKAT_CONTENT_SOURCE`) — an explicit path to a
   `content-source.yaml`.
3. `content-source.yaml` in `<user config dir>/meerkat/` — `os.UserConfigDir()`:
   `$XDG_CONFIG_HOME` or `~/.config` on Linux, `%AppData%` on Windows, and on
   macOS `~/Library/Application Support`, which does **not** consult `XDG_CONFIG_HOME`.
4. `content-source.yaml` in the working directory (wherever `mk`/`meerkat`
   is invoked from — not a repo root).
5. The binary's embedded content — the fallback when none of the above
   apply. Every published artefact (the Homebrew formula, the release
   tarballs, the `ghcr.io` image) is built with no content source, so this
   step serves an empty knowledge base unless you produced the binary
   yourself and [embedded content at build
   time](#optional-embedding-content-at-build-time).

Once a step is used, its `content.type` decides the outcome on its own —
including `type: none`, which resolves to the embedded fallback without
falling through to a lower step.

Steps 2-4 can also mount **several named collections** at once instead of a
single source — see [Multiple collections](#multiple-collections).

### `--kb-dir` / `MEERKAT_KB_DIR`

The simplest way in: point meerkat at a directory on disk and `mk
search`/`show`/`list` serve that content directly, with no config file and
no credentials:

```bash
mk --kb-dir ./meerkat-kb search "rate limiting"
MEERKAT_KB_DIR=./meerkat-kb mk list
```

Precedence: `--kb-dir` flag, then `MEERKAT_KB_DIR` (step 1 above).

The directory uses the **content-repo layout** — the same layout
`content-source.yaml` describes and `mk ingest` writes into:

```text
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
becomes visible with nothing in between ingesting a page and searching it.

A `--kb-dir` that doesn't exist is a hard error (exit 1). A directory that
exists but is missing `wiki/`, `ingestion/`, or `templates/` degrades to
empty for the missing piece — the same answer a content-free binary gives.

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
would be confusing. Only `content.type: none`, `local`, `url`, `gcs` and `s3`
are valid at runtime:

```bash
meerkat --content-source ./content-source.yaml list
MEERKAT_CONTENT_SOURCE=./content-source.yaml meerkat list
```

- **`local`** — a directory on disk, no credentials. A relative `path`
  resolves against **the config file's own directory** — not the working
  directory, and not a repo root. (The build-time resolver differs: it
  resolves against the repo root `make sync` runs from.) An absolute `path`
  behaves the same either way. A resolved directory that doesn't exist is a
  hard error, same as `--kb-dir`. A local source is re-read per request.
- **`url`**, **`gcs`** and **`s3`** fetch, verify and cache a remote
  archive or object prefix — one subsection each, below.
- **`none`** (or no file found at all) serves the binary's embedded content,
  which is empty in every published release.
- **`git`** and **`submodule`** are build-time only (they need git and a
  working tree, which a shipped binary can't assume): naming one here fails
  with an explicit error rather than silently serving nothing. Switch to
  `type: local`/`url`/`gcs`/`s3` for a runtime-resolved source, or
  [embed it at build time](#optional-embedding-content-at-build-time).

A `layout:` block in this file **is** honoured at runtime for `type: local`,
`type: url`, `type: gcs` and `type: s3` sources — unlike `--kb-dir`, above.

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

```text
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

```text
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

### `type: s3`

Loads content from an S3-compatible bucket — AWS S3, or a self-hosted
store such as [Garage](https://garagehq.deuxfleurs.fr/) or
[Versity Gateway](https://github.com/versity/versitygw) — in the same
two modes as `type: gcs`. Runtime-only.

```yaml
# (a) bundle mode — one .tar.gz object, keyed by its ETag
content:
  type: s3
  bucket: my-org-knowledge          # bucket NAME, not an s3:// URL
  object: bundles/kb-v1.2.3.tar.gz
  # etag: "d41d8cd98f00b204e9800998ecf8427e"   # optional: pin one exact ETag

# (b) prefix mode — an object prefix served as a directory tree, on Garage
content:
  type: s3
  bucket: my-org-knowledge
  prefix: kb/live/                  # stripped: kb/live/wiki/x.md -> wiki/x.md
  endpoint: https://s3.example.net  # required for anything that is not AWS
  region: garage                    # Garage checks it against its s3_region
  path_style: true                  # Garage/versitygw: <endpoint>/<bucket>/…; not AWS
```

**Credentials: the AWS default chain, and nothing else.**
`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` in the environment, a
`~/.aws/config` profile, IRSA / web identity on EKS, or instance metadata.
As with `gcs`, there is **no key field in the schema**. Grant the principal
`s3:GetObject`, plus `s3:ListBucket` for prefix mode.

**Caching is by ETag.** S3 has no generation counter; the ETag is the
provider's token for "these bytes", and meerkat treats it as **opaque** —
compared, cached under, and sent back as `If-Match` so the fetch fails
rather than serving a newer object under the old name. It is never
assumed to be a content hash (multipart uploads and SSE-KMS make it
something else), which is why `sha256:` is still available and still
verified before extraction if you set it:

```text
<user cache dir>/meerkat/content/s3/<hash(endpoint,bucket,object)>/<etag>/
<user cache dir>/meerkat/content/s3/<hash(endpoint,bucket,prefix)>/<listing-fingerprint>/
```

In prefix mode the fingerprint covers every listed object's
`(key, ETag, size)`. Setting `etag:` pins a bundle exactly as
`generation:` pins a GCS one. Which providers enforce which
preconditions — and what that means for a `memory:` store on Garage — is
recorded in [docs/design/object-stores.md](docs/design/object-stores.md).

#### `refresh:` — follow the bucket without a restart

By default a GCS or S3 source is resolved **once**, at startup: publishing a new
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

Beyond the examples in ["Use"](#use) above, `mk list --collections --json`
reports each one's name, type, provenance and page count, and `mk show
incidents/paging --collection runbooks` is equivalent to the
`runbooks:incidents/paging` qualified form.

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
— each with its configured `description:`, where there is one — so a client
discovers the set, and what is in each, from the tool list it already
fetches:

```text
Mounted collections: internal — Swish systems and operations;
cra — EU Cyber Resilience Act guidance.
```

A collection with no `description:` is named on its own, exactly as
before, and a single-collection server keeps its one short phrase. On a
hosted server this is rendered from the caller's own visible set, so it
names nothing they may not read. `POST /search`, `/show` and `/list` take
an optional `"collection"` field, and `GET /collections` (auth-gated)
enumerates what's mounted.

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
`method: direct` needs a backend a write can land in (a local directory, or
a GCS or S3 *prefix*) — declaring it on a `type: url` archive, a `gcs`/`s3`
bundle or the embedded fallback fails at **startup**, not at the first lost
write.

What a caller is *told* depends on what that caller may do. Without
`global-write` on a `direct` collection they are pointed at the staging
path ([`mk_save_memory`](#memory-mk_save_memory)) or told there is none —
never at a write they would be refused for. Capabilities only ever narrow
a declared contract: an admin on a `merge-request` collection still opens
a merge request.

Not yet surfaced through the tools — that lands with collection
discovery. Design and rendering rules:
[docs/design/update-contract.md](docs/design/update-contract.md).

### Fuzzy and prefix fallback

A query that finds nothing exactly is retried fuzzily (one edit per
term from 5 characters, two from 8) and then by prefix, so `datadgo
monitor serach` still finds the Datadog page. The stage that answered is
on every hit and on `meerkat_search_total{stage}`; queries that use
search syntax are never rewritten. A hub collection can set
`layout.analyzer: ngram` to index titles as edge n-grams, and any
collection can override the per-`type` score multipliers with
`search.type_boosts` — a leaf that cites through pointers sets
`{pointer: 1.0}`. See [docs/SEARCH.md](docs/SEARCH.md).

### The intake pipeline (`mk ingest --role`)

Research deposited by `mk_report_outcome` becomes knowledge through
three agent roles: `mk ingest --role researcher` turns a raw item into
a candidate page, `--role validator` re-derives its claims (a different
model than the researcher's, two independent confirmations to file),
and `--role librarian` reports dangling links, stale pages, cull
proposals, missing links from the traversal log and parked items —
changing nothing without `--apply`. See
[docs/design/intake.md](docs/design/intake.md).

### Retrieval sessions and SLIs

Calls sharing a `session_id` (or an MCP session) form a retrieval
session: one `meerkat.retrieval.session` span parenting the calls, and
histograms for time to first context, time to first relevant context,
time to give up, hops, steps and wrong turns, plus quality per tier from
`mk_report_outcome`. The tree's traversal limits are enforced per
session with a structured `limit_reached` answer. See
[docs/design/observability.md](docs/design/observability.md#retrieval-sessions-and-slis).

### Reporting outcomes (`mk_report_outcome`)

At the end of a retrieval an agent reports how it went: the outcome,
its first query verbatim, the pages that answered, the collections it
tried, quality scores, and what it did instead when meerkat did not
have it. Every report is counted; with `observability.traversal_log`
configured it is also written as an HMAC-hashed path record, and with an
`intake:` store and the `intake-write` capability a fallback summary
becomes a draft page for review. See
[docs/design/observability.md](docs/design/observability.md#retrieval-outcomes-and-the-traversal-log).

### Lazy mounting and the resident cache (`cache:`)

A `mount: lazy` child of the tree is mounted on the first request that
names it and stays resident under `cache.max_bytes`; past the watermark
the coldest, then oldest-first-traversed, lazy collections are culled
back to cold (content is never touched). `cold_policy: async` answers
`{status: cold, retry_after_ms}` instead of blocking; temperatures are
flushed to the traversal log and read back at startup to warm-start.
See [docs/design/cache.md](docs/design/cache.md).

### The knowledge-base tree (`tree:`)

`tree:` names a root knowledge base whose `manifest.yaml` declares its
children (each an ordinary content source with its own manifest). The
loader mounts every eager child, lists lazy ones as declared but
unmounted, and refuses a tree deeper than five levels, a cycle or a
duplicate name. In a tree, `mk_search` with no collection asks the root
hub only and its pointer pages route; `root/platform/flux` names the
collection at that path. `mk_list_collections` carries `path`, `tier`,
`parent`, `children` and `mounted`. See
[docs/design/tree.md](docs/design/tree.md).

### Links, pointer pages and `mk lint`

`related:` entries are resolved across the mounted collections
(`<id>`, `<collection>:<id>`, or an external `mcp://<server>`), every
page view reports `links` and `linked_from`, and a page of
`type: pointer` with a `target:` (`collection:<name>`,
`<collection>:<id>`, `mcp://<server>`) and a one-sentence `hint:`
routes an agent to the next hop — pointer hits outrank content hits,
and `mk_search` with `bundle: true` groups hits by pointer target into
capability bundles. Dangling links are warnings in `/readyz`, never an
outage; `mk lint` lists them and exits 1 for a content repo's CI. See
[docs/design/links.md](docs/design/links.md).

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
| `s3://<bucket>/<object>@<etag>` | A `type: s3` bundle source — `<etag>` is the ETag actually fetched (opaque; read with `If-Match`). |
| `s3://<bucket>/<prefix>*@<fp>` | A `type: s3` prefix source — `<fp>` fingerprints the listing's `(key, ETag, size)` tuples. |
| `collections:<n>` | Several collections are mounted; each one's own provenance is reported in the `collections` array (see below). |

Like `url:`, the token after `@` on a `gcs:` or `s3:` line is a *checked*
property of what's being served — the conditional read cannot return another
generation or ETag — not a label, unlike `disk:`.

`mk version --json` also carries a `collections` array — `{name, type,
source}` per collection, in configuration order — and the plain-text output
itemises them, so a multi-collection deployment reports everything it
serves. A single-collection deployment reports one entry named `default`,
with `kb_source` unchanged from what it always was.

`kb_commit` is a different field and answers a different question: it names
the commit of the content **embedded at build time**, never a runtime
directory's or archive's. On a published release it is therefore `none` —
the runtime source's provenance is `kb_source`. See
[docs/SECURITY.md](docs/SECURITY.md#kb_commit-vs-kb_source-the-provenance-split)
for what that split means.

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

### Optional: embedding content at build time

A build from source can bake a knowledge base into the binary instead, for a
single artefact that carries its own content. It is **not** how the
published releases are built and nothing above needs it — but it is the only
way to use `type: git` or `type: submodule`, and the only way to get a
`kb_commit` provenance stamp. Put a `content-source.yaml` at the **repo
root** with a build-time type:

```yaml
content:
  type: local            # or: git (repo/host/ref) | submodule
  path: ../your-kb-repo  # relative to the repo root, or absolute
```

`make build` runs `make sync`, which resolves that file into
`internal/kb/content/` and `internal/sources/etc/` before compiling; with no
such file it leaves the committed empty placeholders alone. The resolved
content commit is stamped in and reported by `mk version` as `kb_commit`.
For `type: git`, a private GitHub repo (`host: github`) borrows a cached
`gh` CLI token if one is present; GitLab and other hosts have no credential
borrowing — use a full clone URL / SSH spec or your normal git credential
configuration. Pin `ref` to a tag or SHA; a moving branch warns.

Refreshing means rebuilding: `make build` re-resolves the source, and
`make kb-update` (`git submodule update --remote --recursive`) updates a
`type: submodule` source first. `make sync` will not fetch `type: url`,
`gcs` or `s3` — those are runtime-only. Full schema:
[content-source.example.yaml](content-source.example.yaml),
[docs/design/content-sources.md](docs/design/content-sources.md).

## How search works

Bleve full-text BM25 index, built in-memory at startup from whichever
content was resolved. Title gets ×5 boost, ID gets ×3, body baseline.
Reference measurement from an internal deployment: cold-start ~150 ms on
~700 pages. Your own cold-start time depends on the size of the knowledge
base you point meerkat at.

### Load speed with content from disk

The figure above was measured against content embedded at build time. Point
a published binary at your own knowledge base (see ["Loading
content"](#loading-content)) and the index is built from disk at startup
instead — so the cost lands differently depending on how meerkat is run.

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
make build           # binary in bin/meerkat (+ bin/mk symlink); runs `make sync` first
make test            # go test -race -count=1 ./...
make test-cover      # ... with coverage
make cover-check     # ... and fail below COVERAGE_MIN
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

`.github/workflows/ci.yml` runs on every push to `main` and every pull
request, with no job-to-job dependency: `lint` (golangci-lint, `go mod tidy`
drift, `make docs-check`), `test` (`make cover-check`), `vuln`
(govulncheck), `gosec`, `markdown` (markdownlint over every `*.md`),
`gitleaks`, and `s3-conformance`, a matrix that replays the object-store
assumptions against a real [Garage](https://garagehq.deuxfleurs.fr/) and
[Versity Gateway](https://github.com/versity/versitygw).
`.github/workflows/release.yml` is separate, triggered by a `v*.*.*` tag: a
`verify` job re-runs lint, tests, govulncheck, gosec and gitleaks on the
tagged commit, and only if it passes do `goreleaser` and `docker` run.

The `goreleaser` job cross-builds, generates SPDX SBOMs (via syft), signs
the checksums file with cosign keyless (Fulcio + Rekor), and publishes a
GitHub Release; the `docker` job builds and signs the multi-arch image.
Both run with **no content source**, which is why everything they publish
carries an empty knowledge base by design. See
[docs/RELEASE.md](docs/RELEASE.md) and [docs/INSTALL.md](docs/INSTALL.md)
for the consumer-side verification flow, and ["Embedding content at build
time"](#optional-embedding-content-at-build-time) to bake content into a
binary you build yourself.

Contributions go through a normal fork → branch → pull-request flow against
`main`; there's no direct push. Run the same gates locally first — the
[pre-commit](https://pre-commit.com/) hooks in
[CONTRIBUTING.md](CONTRIBUTING.md) enforce them at commit and push time:

```bash
pre-commit install && pre-commit install --hook-type pre-push   # once per clone
make pre-push          # one-shot: lint + test + docs-check
make pre-release       # ... plus vuln + gosec + gitleaks, before tagging
```

## Repo layout

```text
cmd/
  meerkat/            entrypoint
  meerkat-bootstrap/  standalone installer for a verified upstream release
internal/
  contentsource/  content-source.yaml: runtime resolution (local / url /
                  gcs / s3), build-time sync (local / git / submodule),
                  collections, tree, auth/observability/intake blocks
  contentsync/    `make sync`: the build-time embed tool (not in the binary)
  kbdir/      resolves --kb-dir/MEERKAT_KB_DIR, adapts it onto kb/sources
  kb/         Page + Frontmatter over the resolved (or embedded) content
  collections/    named collections + search/show/list routing across them
  refresh/    opt-in runtime reconciliation: the refresh: controller
  search/     Bleve in-memory BM25 index
  sources/    sources.yaml + prompts + templates
  ingest/     Plan(opts) + Run(ctx, tasks) — planner + executor
  intake/     raw intake store: deposits, candidate pages, parked items
  mcp/        MCP server (mk_search / mk_show / mk_list / mk_list_collections
              / mk_report_outcome / mk_save_memory) — stdio and the hosted
              Streamable HTTP transport, probes, metrics, access log
  retrieval/  retrieval sessions and their SLIs
  traversal/  opt-in traversal log, with page IDs and names HMAC-hashed
  telemetry/  opt-in OpenTelemetry: the observability: block, spans,
              OTLP export, bounded domain metrics. Returns nil when
              nothing opted in, and every method tolerates one
  memory/     writable memory stores (local dir / GCS or S3 prefix) with
              optimistic locking, identity-derived namespaces, staging
  s3api/      the one S3 client shape (content sources + memory stores)
  auth/       borrowed git-host credentials (gh token, OS keyring)
  authn/      OIDC discovery/JWKS verification + the bearer gate (RFC 9728)
  authz/      capability model, access policy, per-collection grants
  http/       HTTP/OpenAPI server with bearer auth
  update/     mk update — gh token, GitHub Releases download, atomic swap
  cli/        cobra command tree; clidocs/ generates docs/CLI.md from it
docs/         CLI.md (generated), INSTALL, CONTAINER, RELEASE, SECURITY,
              SEARCH, OKF, INGESTION, INTEGRATION-*; design/ per subsystem
content-source.yaml   optional, never shipped; tells meerkat at runtime —
                      or `make sync` at build time — where KB content
                      lives (local path / url archive / GCS or S3 object
                      or prefix; git repo or submodule at build time), as
                      one source or several named collections, plus the
                      optional auth: policy, the optional observability:
                      block, per-collection memory: stores and update:
                      contracts
```

## See also

- [zegit.dev/documentation/meerkat.html](https://zegit.dev/documentation/meerkat.html)
  — official meerkat documentation
- [zegit.dev/documentation/meerkat-cli.html](https://zegit.dev/documentation/meerkat-cli.html)
  — CLI reference and integration guides
- [docs/INSTALL.md](docs/INSTALL.md) — install, verify, troubleshoot;
  [docs/CONTAINER.md](docs/CONTAINER.md) — running the OCI image;
  [docs/RELEASE.md](docs/RELEASE.md) — tagging and the release gate
- [docs/SEARCH.md](docs/SEARCH.md) — query syntax and the fallback stages;
  [docs/SECURITY.md](docs/SECURITY.md) — threat model and scanners
- [docs/OKF.md](docs/OKF.md) — serving an [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf) (Open Knowledge Format) bundle unmodified, and what meerkat does with its frontmatter

Design notes, each linked from the section it explains:
[content-sources](docs/design/content-sources.md),
[multi-collection](docs/design/multi-collection.md),
[hot-reload](docs/design/hot-reload.md),
[hosted-mcp](docs/design/hosted-mcp.md),
[observability](docs/design/observability.md),
[memory](docs/design/memory.md),
[update-contract](docs/design/update-contract.md),
[ingestion-pipeline](docs/design/ingestion-pipeline.md),
[intake](docs/design/intake.md),
[tree](docs/design/tree.md),
[cache](docs/design/cache.md),
[links](docs/design/links.md),
[object-stores](docs/design/object-stores.md),
[index-filtering](docs/design/index-filtering.md).

Your own content repo holds the KB pages and `ingestion/sources.yaml` —
meerkat only needs to be pointed at it.
