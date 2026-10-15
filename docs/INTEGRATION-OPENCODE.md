# OpenCode integration

Wire `meerkat` into [OpenCode](https://opencode.ai/) so the agent
can search and read the knowledge base during a coding session — no
context-window bloat, no need to paste pages by hand.

## Add the MCP server

Edit `~/.config/opencode/opencode.json`:

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

Restart OpenCode (or run `opencode reload`). Five tools become
available to the agent, six when a collection declares a `memory:`
store:

| Tool | Args | Returns |
|------|------|---------|
| `mk_search` | `query`, `limit?` (default 10) | `[{id, title, score, snippet, category, status}]` |
| `mk_show` | `id` | `{id, title, body, front, trust_tier, stale}` (raw markdown body + parsed YAML frontmatter, plus two OKF-derived advisory signals — see [OKF.md](OKF.md#trust-and-lifecycle)) |
| `mk_list` | `prefix?`, `category?`, `status?`, `owner?`, `type?` | `[{id, title, category, status, owner, type, source}]` |
| `mk_list_collections` | none | each mounted collection's name, source type, page count and the capabilities you hold on it |
| `mk_report_outcome` | `outcome` (`found`, `not_found`, `gave_up`), `session_id?`, `initial_query?`, `pages?`, `attempted?`, `quality?`, `fallback?` | `{recorded, logged, intake, intake_id?}`: whether the traversal log took the report, and whether a fallback was written to the intake store |
| `mk_save_memory` | see [design/memory.md](design/memory.md) | only registered when a collection declares a `memory:` store |

The agent will pick these up automatically — they appear in
`opencode tools` and are advertised to the model on every prompt.

## Verify

In an OpenCode session:

```text
> Use mk_search to find pages about rate limiting. Return the top 3.

mk_search(query="rate limiting", limit=3)
[1] systems/backend/rate-limiter (score 4.8) — The platform's rate-limiting service …
[2] concepts/Rate-Limiting (score 4.6) — How the platform throttles abusive traffic …
[3] operations/runbooks/rate-limiter (score 4.2) — How to act when rate-limit alerts fire …

> Show me concepts/Rate-Limiting

mk_show(id="concepts/Rate-Limiting")
... full markdown ...
```

The agent's snippet from `mk_search` is enough to triage which
page to `mk_show` next, which keeps the context window lean.

## Recommended usage patterns

- **"What does X mean in our platform"** — agent calls `mk_search` first
  to find the concept page, then `mk_show concepts/<slug>` for the
  definition. Concept pages are designed to be the agent's first
  stop because they cross-link out to systems / ADRs / policies.

- **"What runs in prod with name Y"** — `mk_list --prefix systems/`
  for navigation, `mk_show systems/backend/Y` for details.

- **"Did we already decide on X"** — `mk_search "X"` returns ADR
  hits with snippets. The frontmatter `adr_status` (proposed /
  accepted / superseded) tells the agent how to weight the answer.

- **"What's still TODO in the KB"** — `mk_list --status placeholder`
  surfaces work-queue pages, useful when generating new content
  for them.

## Source provenance — let the agent fetch upstream

Every page's frontmatter carries a `source:` block:

```yaml
source:
  type: gitlab
  repo: your-org/backend/rate-limiter
  ref: HEAD
  path: README.md
  web_url: <your-kb-repo-url>
```

Combined with the bundled `gitlab_glab_*` MCP tools (in OpenCode by
default), an agent can fetch the canonical upstream README rather
than relying solely on the meerkat-rendered summary:

```text
mk_show systems/backend/rate-limiter
  -> see source.repo

gitlab_glab_repo_clone --shallow source.repo
gitlab_glab_api projects/source.repo/repository/files/source.path/raw
```

Use the meerkat page as the **map** and the upstream files as the
**territory**.

## Multiple meerkat instances?

Don't. Both `mk` and `meerkat` share the same MCP entry name
"meerkat" by convention. If you maintain a fork or staging build,
register it under a different MCP name:

```json
"mcp": {
  "meerkat":         { "command": ["mk", "mcp", "serve"], "enabled": true },
  "meerkat-staging": { "command": ["/opt/meerkat-staging/bin/mk", "mcp", "serve"], "enabled": false }
}
```

## Updating

```bash
mk update            # download + verified swap + re-exec
exec opencode reload # OpenCode picks up the new mcp serve binary
```

The binary swap is atomic; existing OpenCode sessions hold an open
file descriptor to the old binary until they exit. New sessions see
the new code.

## Disabling temporarily

```jsonc
"mcp": {
  "meerkat": {
    "command": ["mk", "mcp", "serve"],
    "enabled": false   // <- without removing the entry
  }
}
```

Useful when debugging "is this answer coming from my training data
or from the KB?".

## See also

- `docs/SEARCH.md` — index design + boost ratios
- `docs/INGESTION.md` — how new KB content gets created
- `docs/INTEGRATION-OPENWEBUI.md` — same tools over HTTP for chat
