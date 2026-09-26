# meerkat CLI reference

Auto-generated from the cobra command tree.
Do NOT edit by hand — run `make docs` to regenerate.

Source of truth: `internal/cli/*.go`. Source generator: `internal/clidocs/clidocs.go`.

## Synopsis

```text
Meerkat serves a knowledge-base wiki over CLI subcommands, an MCP
server (for agent harnesses / OpenCode), and an HTTP/OpenAPI server
(for OpenWebUI).

The binary carries no content of its own: it loads a knowledge base at
startup, and search, show and list are then answered locally, with no
service to call. Four mechanisms can say where that knowledge base
lives, consulted highest priority first:

  1. --kb-dir (or MEERKAT_KB_DIR) — an explicit content-repo directory
     (the layout content-source.yaml describes and 'mk ingest' writes
     into). Wins outright over everything below.
  2. --content-source (or MEERKAT_CONTENT_SOURCE) — an explicit path to
     a content-source.yaml.
  3. <user config dir>/meerkat/content-source.yaml
  4. ./content-source.yaml in the working directory
  5. the binary's own embedded content — the fallback when none of the
     above apply. Every published artefact (Homebrew formula, release
     tarballs, ghcr.io image) is built with no content source, so this
     step serves an EMPTY knowledge base unless you built the binary
     yourself with content baked in ('make sync').

'mk version' reports which one won, as kb_source.

content-source.yaml's "content.type" may be none, local, url, gcs or s3
at runtime (git/submodule are build-time only — 'make sync' — since they
need git and a working tree; using one here fails with an error rather
than silently serving nothing). type: local serves a directory on disk,
no credentials. type: url fetches an HTTPS .tar.gz, verifies it against
a required sha256, and caches the extracted result locally, keyed by
that digest. type: gcs and type: s3 load a .tar.gz object or a bucket
prefix from Google Cloud Storage or an S3-compatible store, via the
provider's default credential chain, cached by object version — see
content-source.example.yaml.

A content-source.yaml may instead declare a "collections:" list of
named sources with heterogeneous backends, all mounted at once. Then:

  mk list --collections            enumerate what's mounted
  mk search "term"                 search across every collection
  mk search "term" --collection x  search just collection x
  mk show x:concepts/Thing         a page ID qualified by collection

Page IDs are slash-paths from the wiki root without ".md" — e.g.
"concepts/Some-Concept", "systems/backend/some-service" — optionally
prefixed with "<collection>:" when several collections are mounted.

Short alias: 'mk' (installed as a symlink alongside meerkat).
```

## Examples

```sh
# Point meerkat at a knowledge base — nothing is served until you do
  meerkat --kb-dir ./meerkat-kb search "some term"
  MEERKAT_KB_DIR=./meerkat-kb meerkat list

  # ... or resolve it from a content-source.yaml (type: none|local|url|gcs|s3)
  meerkat --content-source ./content-source.yaml list

  # Knowledge base (answered locally, no service to call)
  meerkat search "some term"
  meerkat show concepts/Some-Concept
  meerkat list --prefix systems/backend/
  meerkat list --category policies --status placeholder

  # Multiple named collections mounted at once
  meerkat list --collections
  meerkat search "incident" --collection runbooks
  meerkat show runbooks/index --collection runbooks
```

## Commands

### Knowledge base (answered locally, no service to call)

- [`meerkat lint`](#meerkat-lint) — Check related: links and pointer targets across the mounted collections
- [`meerkat list`](#meerkat-list) — List wiki pages, optionally filtered
- [`meerkat search`](#meerkat-search) — Full-text search across the loaded knowledge base
- [`meerkat show`](#meerkat-show) — Print a single wiki page

### Servers

- [`meerkat http`](#meerkat-http) — Run an HTTP/OpenAPI server
- [`meerkat mcp`](#meerkat-mcp) — Run an MCP (Model Context Protocol) server

### Operations

- [`meerkat ingest`](#meerkat-ingest) — Plan and execute ingestion of placeholder KB pages
- [`meerkat update`](#meerkat-update) — Check for or install meerkat updates
- [`meerkat version`](#meerkat-version) — Print version information

## Reference

### `meerkat`

Meerkat — the vigilant guard and informer (knowledge-base CLI)

```text
Meerkat serves a knowledge-base wiki over CLI subcommands, an MCP
server (for agent harnesses / OpenCode), and an HTTP/OpenAPI server
(for OpenWebUI).

The binary carries no content of its own: it loads a knowledge base at
startup, and search, show and list are then answered locally, with no
service to call. Four mechanisms can say where that knowledge base
lives, consulted highest priority first:

  1. --kb-dir (or MEERKAT_KB_DIR) — an explicit content-repo directory
     (the layout content-source.yaml describes and 'mk ingest' writes
     into). Wins outright over everything below.
  2. --content-source (or MEERKAT_CONTENT_SOURCE) — an explicit path to
     a content-source.yaml.
  3. <user config dir>/meerkat/content-source.yaml
  4. ./content-source.yaml in the working directory
  5. the binary's own embedded content — the fallback when none of the
     above apply. Every published artefact (Homebrew formula, release
     tarballs, ghcr.io image) is built with no content source, so this
     step serves an EMPTY knowledge base unless you built the binary
     yourself with content baked in ('make sync').

'mk version' reports which one won, as kb_source.

content-source.yaml's "content.type" may be none, local, url, gcs or s3
at runtime (git/submodule are build-time only — 'make sync' — since they
need git and a working tree; using one here fails with an error rather
than silently serving nothing). type: local serves a directory on disk,
no credentials. type: url fetches an HTTPS .tar.gz, verifies it against
a required sha256, and caches the extracted result locally, keyed by
that digest. type: gcs and type: s3 load a .tar.gz object or a bucket
prefix from Google Cloud Storage or an S3-compatible store, via the
provider's default credential chain, cached by object version — see
content-source.example.yaml.

A content-source.yaml may instead declare a "collections:" list of
named sources with heterogeneous backends, all mounted at once. Then:

  mk list --collections            enumerate what's mounted
  mk search "term"                 search across every collection
  mk search "term" --collection x  search just collection x
  mk show x:concepts/Thing         a page ID qualified by collection

Page IDs are slash-paths from the wiki root without ".md" — e.g.
"concepts/Some-Concept", "systems/backend/some-service" — optionally
prefixed with "<collection>:" when several collections are mounted.

Short alias: 'mk' (installed as a symlink alongside meerkat).
```

#### Usage

```text
meerkat
```

#### Subcommands

- `http` — Run an HTTP/OpenAPI server
- `ingest` — Plan and execute ingestion of placeholder KB pages
- `lint` — Check related: links and pointer targets across the mounted collections
- `list` — List wiki pages, optionally filtered
- `mcp` — Run an MCP (Model Context Protocol) server
- `search` — Full-text search across the loaded knowledge base
- `show` — Print a single wiki page
- `update` — Check for or install meerkat updates
- `version` — Print version information

#### Flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

#### Examples

```sh
# Point meerkat at a knowledge base — nothing is served until you do
  meerkat --kb-dir ./meerkat-kb search "some term"
  MEERKAT_KB_DIR=./meerkat-kb meerkat list

  # ... or resolve it from a content-source.yaml (type: none|local|url|gcs|s3)
  meerkat --content-source ./content-source.yaml list

  # Knowledge base (answered locally, no service to call)
  meerkat search "some term"
  meerkat show concepts/Some-Concept
  meerkat list --prefix systems/backend/
  meerkat list --category policies --status placeholder

  # Multiple named collections mounted at once
  meerkat list --collections
  meerkat search "incident" --collection runbooks
  meerkat show runbooks/index --collection runbooks
```

---

### `meerkat http`

Run an HTTP/OpenAPI server

```text
Serve the meerkat KB over HTTP for OpenWebUI tool servers and
similar clients. The endpoint surface mirrors MCP 1:1.
```

#### Usage

```text
meerkat http
```

#### Subcommands

- `serve` — Serve the meerkat KB tools over HTTP/JSON with bearer auth

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat http serve`

Serve the meerkat KB tools over HTTP/JSON with bearer auth

```text
Run an HTTP/OpenAPI server. Endpoints:

  POST /search        full-text search
  POST /show          retrieve one page (body + frontmatter)
  POST /list          enumerate pages with filters
  GET  /collections   enumerate the mounted collections
  GET  /openapi.json  schema (no auth)
  GET  /healthz       liveness (no auth)

/search, /show and /list take an optional "collection" field; omitted,
they span every mounted collection.

Authentication: all data endpoints require an Authorization: Bearer
header carrying the configured API key. The key is supplied via
--api-key or the MEERKAT_API_KEY env var (env wins if both set).
The server refuses to start without a key — there is no anonymous
mode.

Register http://<host>:<port>/openapi.json with OpenWebUI as a Tool
Server.
```

#### Usage

```text
meerkat http serve [flags]
```

#### Flags

```text
      --api-key string   Static bearer token. Required (or set MEERKAT_API_KEY).
      --host string      Bind host (use 0.0.0.0 to listen on all interfaces) (default "127.0.0.1")
      --port int         Bind port (default 4004)
```

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat ingest`

Plan and execute ingestion of placeholder KB pages

```text
Drive Meerkat's ingestion pipeline.

The default behaviour is plan-only: print the JSONL batch that
would be executed. Add --execute to spawn one opencode session
per page (the actual ingestion).

Examples:
  mk ingest --source policies               # plan only, all stale policy pages
  mk ingest --source policies --execute     # run them
  mk ingest --page concepts/Rate-Limiting --execute
  mk ingest --execute --max-parallel 4      # everything stale, 4-wide
  mk ingest --batch-file batch.jsonl        # plan to file, no execute
```

#### Usage

```text
meerkat ingest [flags]
```

#### Subcommands

- `sources` — List the ingestion source registry

#### Flags

```text
      --apply                          With --role librarian: file confirmed candidates through their collection's contract. Without it the librarian changes nothing.
      --batch-file string              Plan-only: write the JSONL batch to this file instead of stdout.
      --branch string                  Push target branch. Overrides the branch derived from content-source.yaml.
      --days int                       With --role librarian: days of traversal log to read for missing-link findings. (default 7)
      --dry-run                        With --execute, print the planned commands without running them.
      --execute                        Actually run the tasks (otherwise plan-only).
      --executor string                Agent CLI to run each page: opencode | claude. (default "opencode")
      --from string                    With --role researcher|validator: where items come from (intake, the content-source.yaml intake: store). (default "intake")
      --max-consecutive-failures int   Stop the executor after this many consecutive failures (0 = never auto-stop).
      --max-parallel int               Max concurrent opencode sessions when --execute (default 1). (default 1)
      --model string                   Model override (default: openai/gpt-5.5-fast or per-source override in sources.yaml).
      --namespace string               With --role researcher: only this identity namespace's deposits (default: every namespace).
      --only string                    With --role: only this intake id.
      --page string                    Plan exactly one page (e.g. 'concepts/Rate-Limiting' or 'wiki/policies/foo.md'). Overrides --source/--statuses.
      --reverse                        Process tasks in reverse ID order. Useful when running a second batch alongside a forward one — collisions skip cheaply because the executor checks page status before spawning opencode.
      --role string                    Act as an intake role: researcher (raw intake -> candidate page), validator (re-derive a candidate's claims), librarian (report on the tree; --apply files confirmed candidates).
      --source string                  Restrict to one source id from sources.yaml (e.g. policies, adr, runbooks).
      --status strings                 Restrict to pages with these frontmatter statuses (default: placeholder,ingest-failed).
      --subagent string                OpenCode subagent type (default: general).
      --trust-sources                  Run the agent CLI with permission prompts disabled; any instruction reachable from ingested content then executes unchallenged.
      --wall-clock-cap int             Per-page wall-clock cap in seconds. (default 300)
      --workdir-kb string              Content working copy to write to. Overrides the source resolved from content-source.yaml.
```

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat ingest sources`

List the ingestion source registry

```text
Print every source from the loaded knowledge base's
ingestion/sources.yaml.
```

#### Usage

```text
meerkat ingest sources [flags]
```

#### Flags

```text
      --json   Output as JSON
```

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat lint`

Check related: links and pointer targets across the mounted collections

```text
Resolve every page's related: entries and every type: pointer page's
target: against the mounted collections, and list the ones that do not
resolve.

A link is dangling when it names a page that does not exist
("systems/api", "flux:concepts/drift"), a collection that is not mounted
("collection:flux", "flux:…"), or does not parse at all. A pointer is
invalid when it has no target:, no hint:, or targets a page in its own
collection (use related: for that). External references (mcp://…,
ext:<scheme>:<target>, external:<name>) are never checked.

Exit status is 1 when anything dangles, so a content repository can run
this in CI. The same findings are reported, capped, as warnings in
/readyz and mk_list_collections; they never make a collection unready.
```

#### Usage

```text
meerkat lint [flags]
```

#### Flags

```text
      --json   Output the full report as JSON
```

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat list`

List wiki pages, optionally filtered

```text
List the pages in the loaded knowledge base.

Filters compose (AND):
  --prefix    page ID prefix, e.g. "systems/backend/"
  --category  frontmatter 'category' field, e.g. "policies"
  --status    frontmatter 'status' field, e.g. "placeholder", "reviewed"
  --owner     frontmatter 'owner' field, e.g. "team-payments"
  --type      frontmatter 'type' field, e.g. "BigQuery Table" (OKF's
              required concept-kind field, SPEC.md §4.1)

With several collections mounted, pages from all of them are listed in
configuration order and IDs print qualified ("<collection>:<page-id>");
--collection narrows the listing to one. --collections instead
enumerates the mounted collections themselves.

Default output is "id  title  status". --json adds frontmatter.
```

#### Usage

```text
meerkat list [flags]
```

#### Flags

```text
      --category string     Only pages with this frontmatter category
      --collection string   Only list this named collection (see 'mk list --collections'). Default: every mounted collection. Single-collection deployments can ignore this.
      --collections         List the mounted collections (name, type, provenance, page count) instead of pages
      --json                Output as JSON (includes frontmatter)
      --owner string        Only pages with this frontmatter owner
      --prefix string       Only pages whose ID starts with this prefix
      --status string       Only pages with this frontmatter status
      --type string         Only pages with this frontmatter type (OKF's concept-kind field, e.g. "BigQuery Table")
```

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat mcp`

Run an MCP (Model Context Protocol) server

```text
Manage MCP servers exposing the meerkat KB.

Two transports serve the identical tool set:

  mcp serve       stdio — spawned by a local MCP client, no auth
  mcp serve-http  Streamable HTTP — hosted, concurrent, OIDC-authenticated

Wire the stdio server into OpenCode by adding to
~/.config/opencode/opencode.json:

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

#### Usage

```text
meerkat mcp
```

#### Subcommands

- `serve` — Serve the meerkat KB tools over MCP/stdio
- `serve-http` — Serve the meerkat KB tools over MCP Streamable HTTP with OIDC auth

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat mcp serve`

Serve the meerkat KB tools over MCP/stdio

```text
Run a Model Context Protocol server on stdio. Exposes:

  mk_search       - full-text search across the loaded KB
  mk_show         - retrieve one page by ID (returns body + frontmatter)
  mk_list         - list pages, optionally filtered (prefix/category/status/owner)
  mk_save_memory  - save a personal/team/global memory, searchable at once
                    (only when a collection declares a "memory:" store)

Every tool takes an optional "collection" argument; with several
collections mounted, each tool's description names them, so a client
discovers the set from the tool list it already fetches.

stdio is unauthenticated by construction — the process was started by
the one user it serves — so personal memories saved here land in a fixed
"local" namespace rather than one derived from a token.

Designed to be spawned by an MCP client (OpenCode, Claude Desktop, etc.).
The server runs until stdin closes or it receives SIGINT/SIGTERM.
```

#### Usage

```text
meerkat mcp serve
```

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat mcp serve-http`

Serve the meerkat KB tools over MCP Streamable HTTP with OIDC auth

```text
Run a hosted Model Context Protocol server on the Streamable HTTP
transport. It exposes the same tools as 'mcp serve':

  mk_search       - full-text search across the KB
  mk_show         - retrieve one page by ID (returns body + frontmatter)
  mk_list         - list pages, optionally filtered
  mk_save_memory  - save a personal/team/global memory (only when a
                    collection declares a "memory:" store)

Endpoints:

  /mcp                                    MCP Streamable HTTP (POST/GET/DELETE)
  /.well-known/oauth-protected-resource   RFC 9728 metadata (no auth)
  /livez                                  liveness probe (no auth)
  /readyz                                 readiness: content + index health (no auth)
  /metrics                                Prometheus metrics (no auth)

Authentication and authorization are configured by an "auth:" block in
content-source.yaml, or by a standalone file passed with --auth-config:

  auth:
    resource: https://mcp.example.com/mcp
    providers:
      - issuer: https://login.microsoftonline.com/<tenant>/v2.0
        audience: api://meerkat
        claims: { groups: groups, email: preferred_username, tenant: tid }
    rules:
      - name: sre
        groups: [sre]
        collections: [runbooks]
        capabilities: [read]

With providers configured, every request to /mcp must carry a verified
OIDC bearer token; one that doesn't gets 401 with a WWW-Authenticate
header pointing at the metadata endpoint. Each caller then sees ONLY
the collections their rules grant 'read' on — the rest are invisible,
not merely denied: they are absent from tool descriptions, from search
and list results, from the collection named in an error message, and
from show's ambiguity resolution.

mk_save_memory is gated the same way, on the write capabilities
(personal-write / team-write / global-write) rather than on read: a
caller holding none of them anywhere is not offered the tool at all. A
personal memory's namespace comes from the verified token's subject and
issuer — there is no argument that could name anybody else. A team or
global memory a caller may not write is saved as a pending review
artifact under the store's _staging/ prefix, which is never indexed or
served. See docs/design/memory.md.

Selected collections can be published to callers with NO token at all,
with an "anonymous: true" rule:

  rules:
    - name: public-handbook
      anonymous: true
      collections: [handbook]
      capabilities: [read]         # anonymous rules are read-only

Everything else still requires a verified token. An anonymous caller
sees exactly the published collections and nothing else — the rest stay
indistinguishable from collections this deployment never mounted — and
is offered no write tool and owns no personal memories. A token that is
present but expired, malformed or forged is still 401: it is NEVER
downgraded to anonymous access, because an expiry that silently degrades
into partial data is an outage nobody sees. Cannot be combined with
allow_unauthenticated.

With NO auth: block configured the server is unauthenticated and every
mounted collection is readable by any caller — the same posture as
'mcp serve'. Bind loopback (the default) or put a gateway in front.

A collection whose content-source.yaml entry carries a "refresh:" block
is re-checked while the server runs: a metadata-only probe every
interval, and — only when the GCS object generation or prefix
fingerprint actually moved — a re-resolve, an off-request-path index
rebuild and an atomic swap. Queries keep being served throughout, from
the previous snapshot until the new one is complete. A "refresh:" block
under a collection's "memory:" is the same thing for a shared GCS memory
store, and is what makes several replicas converge on each other's
writes. SIGHUP runs every configured refresh immediately. See
docs/design/hot-reload.md.

An "observability:" block in content-source.yaml (or the standard OTEL_*
environment variables) turns on OpenTelemetry tracing and optional OTLP
export: one mk_search then becomes one trace spanning the HTTP request,
OIDC verification, the authorization decision, the tool call, the search
and any GCS or memory work underneath it, and the access log gains
matching trace_id/span_id. With no block and no OTEL_* variable nothing
is constructed at all — no spans, no exporter, no socket — and /metrics
and the JSON logs are exactly what they were. Spans carry counts,
durations and closed-set outcomes only: never a query, a page ID, a
collection name, a bucket, a token or a subject. A collector that is
down never affects a request, /readyz or shutdown. See
docs/design/observability.md.

The server has no TLS of its own; terminate TLS at a reverse proxy.
```

#### Usage

```text
meerkat mcp serve-http [flags]
```

#### Flags

```text
      --auth-config string   Path to a standalone YAML policy file with a top-level auth: block. Overrides the auth: block in content-source.yaml.
      --host string          Bind host (use 0.0.0.0 to listen on all interfaces) (default "127.0.0.1")
      --path string          Path the MCP Streamable HTTP endpoint is mounted at (default "/mcp")
      --port int             Bind port (default 4005)
      --stateful             Keep per-session state in this process instead of accepting any well-formed session ID. Requires sticky routing when more than one replica sits behind a load balancer.
      --trust-proxy-host     Disable DNS-rebinding protection (which rejects loopback requests whose Host header is not a localhost value). Only for a same-host reverse proxy that preserves the original Host header; prefer rewriting Host at the proxy instead.
```

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat search`

Full-text search across the loaded knowledge base

```text
Run a full-text keyword search over every page in the loaded knowledge
base.

Title and ID matches are boosted so page-name lookups (e.g. "onboarding",
"rate-limiting") rank above incidental body mentions.

With several collections mounted, every collection is searched and the
hits are merged by score; --collection narrows it to one. Result IDs are
printed qualified ("<collection>:<page-id>") whenever more than one
collection is mounted, so they can be pasted straight into 'mk show'.

Examples:
  mk search "rate limiting"
  mk search "retention policy"
  mk search title:eviction        # field-targeted query
  mk search "30 minute" --limit 20
  mk search "incident" --collection runbooks
```

#### Usage

```text
meerkat search <query> [flags]
```

#### Flags

```text
      --body                Print the full body of every hit
      --collection string   Only search this named collection (see 'mk list --collections'). Default: every mounted collection. Single-collection deployments can ignore this.
      --json                Output results as JSON
      --limit int           Maximum number of results (default 10)
```

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat show`

Print a single wiki page

```text
Print the raw markdown for a single wiki page.

Page IDs are slash-separated paths from the wiki root, without the .md
suffix. Examples:
  mk show index
  mk show concepts/rate-limiting
  mk show systems/backend/access

With several collections mounted, every collection is tried in
configuration order. A page ID may be qualified as
"<collection>:<page-id>", or narrowed with --collection; a bare ID that
exists in more than one collection is an error listing the qualified
IDs to choose from, never a silent pick:
  mk show runbooks:incidents/paging
  mk show incidents/paging --collection runbooks

--json adds two OKF-derived advisory signals alongside the page's own
frontmatter (front): trust_tier (unverified | machine-confirmed |
human-reviewed, derived from front.verified — SPEC.md §5.3) and stale
(whether today is on/after front.stale_after — SPEC.md §5.5), plus the
collection the page was served from.
```

#### Usage

```text
meerkat show <page-id> [flags]
```

#### Flags

```text
      --collection string   Only look in this named collection (see 'mk list --collections'). Default: every mounted collection. Single-collection deployments can ignore this.
      --json                Output as JSON (page metadata + body)
```

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat update`

Check for or install meerkat updates

```text
Check the GitHub Releases page for newer meerkat versions and,
unless --check is given, download + atomically swap the binary
+ re-exec.

The repository is public, so this works with no authentication at
all. If you've run 'gh auth login', meerkat borrows the cached
token from gh's OAuth credential cache and sends it for the higher
authenticated GitHub API rate limit — there are no PATs to paste
either way.

Installed via Homebrew (brew install zegit-zoo/tap/meerkat)? The
binary lives in the Cellar and belongs to brew, so an in-place swap
would be undone by the next 'brew upgrade'. This command refuses to
touch such an install; run 'brew update && brew upgrade meerkat'
instead. --check
still works everywhere — it only reads.

Examples:
  mk update --check                  # just print latest version
  mk update                          # interactive: prompt before swap
  mk update --yes                    # download + swap without prompt
  mk update --version v0.4.0         # pin to a specific tag
  mk update --force                  # downgrade or re-install same
```

#### Usage

```text
meerkat update [flags]
```

#### Flags

```text
      --check            Just report the latest version; don't download or install.
      --force            Re-install even if already on the target version (downgrade-friendly).
      --skip-cosign      Skip cosign signature verification (NOT recommended — sha256-only).
      --version string   Install a specific tag (e.g. v0.4.0) instead of latest.
  -y, --yes              Skip the confirmation prompt.
```

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat version`

Print version information

#### Usage

```text
meerkat version [flags]
```

#### Flags

```text
      --json   Output as JSON
```

#### Inherited flags

```text
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs|s3, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the binary's embedded content (empty in every published release).
      --kb-dir string           Serve KB content from this directory at runtime (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/ — the layout 'mk ingest' writes into). Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---
