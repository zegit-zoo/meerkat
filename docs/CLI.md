# meerkat CLI reference

Auto-generated from the cobra command tree.
Do NOT edit by hand — run `make docs` to regenerate.

Source of truth: `internal/cli/*.go`. Source generator: `internal/clidocs/clidocs.go`.

## Synopsis

```
Meerkat embeds a knowledge-base wiki and exposes it via
CLI subcommands, an MCP server (for agent harnesses / OpenCode), and an
HTTP/OpenAPI server (for OpenWebUI).

All wiki content is bundled into the binary at build time and served
from there by default. No network access is required for search,
show, or list. What gets served instead is resolved in priority order:

  1. --kb-dir (or MEERKAT_KB_DIR) — an explicit content-repo directory
     (the layout content-source.yaml describes and 'mk ingest' writes
     into). Wins outright over everything below.
  2. --content-source (or MEERKAT_CONTENT_SOURCE) — an explicit path to
     a content-source.yaml.
  3. <user config dir>/meerkat/content-source.yaml
  4. ./content-source.yaml in the working directory
  5. the embedded build (the fallback when none of the above apply)

content-source.yaml's "content.type" may be none, local, url or gcs at
runtime (git/submodule are build-time only — 'make sync' — since they
need git and a working tree; using one here fails with an error rather
than silently serving nothing). type: url fetches an HTTPS .tar.gz,
verifies it against a required sha256, and caches the extracted result
locally, keyed by that digest. type: gcs loads a Google Cloud Storage
.tar.gz object or bucket prefix, authenticating via Application Default
Credentials and caching by object generation — see
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
# Knowledge base (offline)
  meerkat search "some term"
  meerkat show concepts/Some-Concept
  meerkat list --prefix systems/backend/
  meerkat list --category policies --status placeholder

  # Serve content from disk instead of the embedded build
  meerkat --kb-dir ./meerkat-kb search "some term"
  MEERKAT_KB_DIR=./meerkat-kb meerkat list

  # Serve content resolved from a content-source.yaml (type: none|local|url|gcs)
  meerkat --content-source ./content-source.yaml list

  # Multiple named collections mounted at once
  meerkat list --collections
  meerkat search "incident" --collection runbooks
  meerkat show runbooks/index --collection runbooks
```

## Commands

### Knowledge base (always available, offline)

- [`meerkat list`](#meerkat-list) — List wiki pages, optionally filtered
- [`meerkat search`](#meerkat-search) — Full-text search across the embedded wiki
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

Meerkat embeds a knowledge-base wiki and exposes it via
CLI subcommands, an MCP server (for agent harnesses / OpenCode), and an
HTTP/OpenAPI server (for OpenWebUI).

All wiki content is bundled into the binary at build time and served
from there by default. No network access is required for search,
show, or list. What gets served instead is resolved in priority order:

  1. --kb-dir (or MEERKAT_KB_DIR) — an explicit content-repo directory
     (the layout content-source.yaml describes and 'mk ingest' writes
     into). Wins outright over everything below.
  2. --content-source (or MEERKAT_CONTENT_SOURCE) — an explicit path to
     a content-source.yaml.
  3. <user config dir>/meerkat/content-source.yaml
  4. ./content-source.yaml in the working directory
  5. the embedded build (the fallback when none of the above apply)

content-source.yaml's "content.type" may be none, local, url or gcs at
runtime (git/submodule are build-time only — 'make sync' — since they
need git and a working tree; using one here fails with an error rather
than silently serving nothing). type: url fetches an HTTPS .tar.gz,
verifies it against a required sha256, and caches the extracted result
locally, keyed by that digest. type: gcs loads a Google Cloud Storage
.tar.gz object or bucket prefix, authenticating via Application Default
Credentials and caching by object generation — see
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

**Usage**

```
meerkat
```

**Subcommands**

- `http` — Run an HTTP/OpenAPI server
- `ingest` — Plan and execute ingestion of placeholder KB pages
- `list` — List wiki pages, optionally filtered
- `mcp` — Run an MCP (Model Context Protocol) server
- `search` — Full-text search across the embedded wiki
- `show` — Print a single wiki page
- `update` — Check for or install meerkat updates
- `version` — Print version information

**Flags**

```
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the embedded build.
      --kb-dir string           Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

**Examples**

```sh
# Knowledge base (offline)
  meerkat search "some term"
  meerkat show concepts/Some-Concept
  meerkat list --prefix systems/backend/
  meerkat list --category policies --status placeholder

  # Serve content from disk instead of the embedded build
  meerkat --kb-dir ./meerkat-kb search "some term"
  MEERKAT_KB_DIR=./meerkat-kb meerkat list

  # Serve content resolved from a content-source.yaml (type: none|local|url|gcs)
  meerkat --content-source ./content-source.yaml list

  # Multiple named collections mounted at once
  meerkat list --collections
  meerkat search "incident" --collection runbooks
  meerkat show runbooks/index --collection runbooks
```

---

### `meerkat http`

Run an HTTP/OpenAPI server

Serve the meerkat KB over HTTP for OpenWebUI tool servers and
similar clients. The endpoint surface mirrors MCP 1:1.

**Usage**

```
meerkat http
```

**Subcommands**

- `serve` — Serve the meerkat KB tools over HTTP/JSON with bearer auth

**Inherited flags**

```
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the embedded build.
      --kb-dir string           Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat http serve`

Serve the meerkat KB tools over HTTP/JSON with bearer auth

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

**Usage**

```
meerkat http serve [flags]
```

**Flags**

```
      --api-key string   Static bearer token. Required (or set MEERKAT_API_KEY).
      --host string      Bind host (use 0.0.0.0 to listen on all interfaces) (default "127.0.0.1")
      --port int         Bind port (default 4004)
```

**Inherited flags**

```
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the embedded build.
      --kb-dir string           Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat ingest`

Plan and execute ingestion of placeholder KB pages

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

**Usage**

```
meerkat ingest [flags]
```

**Subcommands**

- `sources` — List the embedded ingestion source registry

**Flags**

```
      --batch-file string              Plan-only: write the JSONL batch to this file instead of stdout.
      --branch string                  Push target branch. Overrides the branch derived from content-source.yaml.
      --dry-run                        With --execute, print the planned commands without running them.
      --execute                        Actually run the tasks (otherwise plan-only).
      --executor string                Agent CLI to run each page: opencode | claude. (default "opencode")
      --max-consecutive-failures int   Stop the executor after this many consecutive failures (0 = never auto-stop).
      --max-parallel int               Max concurrent opencode sessions when --execute (default 1). (default 1)
      --model string                   Model override (default: openai/gpt-5.5-fast or per-source override in sources.yaml).
      --page string                    Plan exactly one page (e.g. 'concepts/Rate-Limiting' or 'wiki/policies/foo.md'). Overrides --source/--statuses.
      --reverse                        Process tasks in reverse ID order. Useful when running a second batch alongside a forward one — collisions skip cheaply because the executor checks page status before spawning opencode.
      --source string                  Restrict to one source id from sources.yaml (e.g. policies, adr, runbooks).
      --status strings                 Restrict to pages with these frontmatter statuses (default: placeholder,ingest-failed).
      --subagent string                OpenCode subagent type (default: general).
      --trust-sources                  Run the agent CLI with permission prompts disabled; any instruction reachable from ingested content then executes unchallenged.
      --wall-clock-cap int             Per-page wall-clock cap in seconds. (default 300)
      --workdir-kb string              Content working copy to write to. Overrides the source resolved from content-source.yaml.
```

**Inherited flags**

```
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the embedded build.
      --kb-dir string           Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat ingest sources`

List the embedded ingestion source registry

Print every source from the embedded sources.yaml.

**Usage**

```
meerkat ingest sources [flags]
```

**Flags**

```
      --json   Output as JSON
```

**Inherited flags**

```
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the embedded build.
      --kb-dir string           Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat list`

List wiki pages, optionally filtered

List pages embedded in this meerkat binary.

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

**Usage**

```
meerkat list [flags]
```

**Flags**

```
      --category string     Only pages with this frontmatter category
      --collection string   Only list this named collection (see 'mk list --collections'). Default: every mounted collection. Single-collection deployments can ignore this.
      --collections         List the mounted collections (name, type, provenance, page count) instead of pages
      --json                Output as JSON (includes frontmatter)
      --owner string        Only pages with this frontmatter owner
      --prefix string       Only pages whose ID starts with this prefix
      --status string       Only pages with this frontmatter status
      --type string         Only pages with this frontmatter type (OKF's concept-kind field, e.g. "BigQuery Table")
```

**Inherited flags**

```
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the embedded build.
      --kb-dir string           Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat mcp`

Run an MCP (Model Context Protocol) server

Manage MCP servers exposing the meerkat KB.

Wire into OpenCode by adding to ~/.config/opencode/opencode.json:

  {
    "mcp": {
      "meerkat": {
        "type": "local",
        "command": ["mk", "mcp", "serve"],
        "enabled": true
      }
    }
  }

**Usage**

```
meerkat mcp
```

**Subcommands**

- `serve` — Serve the meerkat KB tools over MCP/stdio

**Inherited flags**

```
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the embedded build.
      --kb-dir string           Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat mcp serve`

Serve the meerkat KB tools over MCP/stdio

Run a Model Context Protocol server on stdio. Exposes:

  mk_search  - full-text search across the embedded KB
  mk_show    - retrieve one page by ID (returns body + frontmatter)
  mk_list    - list pages, optionally filtered (prefix/category/status/owner)

Every tool takes an optional "collection" argument; with several
collections mounted, each tool's description names them, so a client
discovers the set from the tool list it already fetches.

Designed to be spawned by an MCP client (OpenCode, Claude Desktop, etc.).
The server runs until stdin closes or it receives SIGINT/SIGTERM.

**Usage**

```
meerkat mcp serve
```

**Inherited flags**

```
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the embedded build.
      --kb-dir string           Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat search`

Full-text search across the embedded wiki

Run a BM25 full-text search over every embedded wiki page.

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

**Usage**

```
meerkat search <query> [flags]
```

**Flags**

```
      --body                Print the full body of every hit
      --collection string   Only search this named collection (see 'mk list --collections'). Default: every mounted collection. Single-collection deployments can ignore this.
      --json                Output results as JSON
      --limit int           Maximum number of results (default 10)
```

**Inherited flags**

```
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the embedded build.
      --kb-dir string           Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat show`

Print a single wiki page

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

**Usage**

```
meerkat show <page-id> [flags]
```

**Flags**

```
      --collection string   Only look in this named collection (see 'mk list --collections'). Default: every mounted collection. Single-collection deployments can ignore this.
      --json                Output as JSON (page metadata + body)
```

**Inherited flags**

```
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the embedded build.
      --kb-dir string           Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat update`

Check for or install meerkat updates

Check the GitHub Releases page for newer meerkat versions and,
unless --check is given, download + atomically swap the binary
+ re-exec.

The repository is public, so this works with no authentication at
all. If you've run 'gh auth login', meerkat borrows the cached
token from gh's OAuth credential cache and sends it for the higher
authenticated GitHub API rate limit — there are no PATs to paste
either way.

Examples:
  mk update --check                  # just print latest version
  mk update                          # interactive: prompt before swap
  mk update --yes                    # download + swap without prompt
  mk update --version v0.4.0         # pin to a specific tag
  mk update --force                  # downgrade or re-install same

**Usage**

```
meerkat update [flags]
```

**Flags**

```
      --check            Just report the latest version; don't download or install.
      --force            Re-install even if already on the target version (downgrade-friendly).
      --skip-cosign      Skip cosign signature verification (NOT recommended — sha256-only).
      --version string   Install a specific tag (e.g. v0.4.0) instead of latest.
  -y, --yes              Skip the confirmation prompt.
```

**Inherited flags**

```
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the embedded build.
      --kb-dir string           Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

### `meerkat version`

Print version information

**Usage**

```
meerkat version [flags]
```

**Flags**

```
      --json   Output as JSON
```

**Inherited flags**

```
      --content-source string   Path to a content-source.yaml describing where to serve KB content from (content.type: none|local|url|gcs, or a collections: list of named sources — git/submodule are build-time-only, 'make sync'). Overrides MEERKAT_CONTENT_SOURCE. Loses to --kb-dir/MEERKAT_KB_DIR. When neither this nor --kb-dir/MEERKAT_KB_DIR is set, falls back to <user-config-dir>/meerkat/content-source.yaml, then ./content-source.yaml, then the embedded build.
      --kb-dir string           Serve KB content from this directory (content-repo layout: wiki/, ingestion/sources.yaml, ingestion/prompts/, templates/) instead of the embedded build. Overrides MEERKAT_KB_DIR. The directory must exist; a missing wiki/ingestion/templates subdirectory inside it degrades to empty rather than erroring. Wins over --content-source / content-source.yaml discovery below.
```

---

