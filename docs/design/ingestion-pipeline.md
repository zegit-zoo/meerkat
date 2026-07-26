# Spec: Generalized ingestion pipeline

**Status:** Implemented (Executor interface + opencode & claude agent-CLI executors; routing, branch/workdir derivation) · **Builds on:** [content-sources.md](content-sources.md)

## Summary

`mk ingest` plans and runs the agent-driven population of placeholder KB
pages: a **planner** walks the embedded pages, matches each to a source in
`sources.yaml`, renders its prompt, and emits one JSONL `Task`; an
**executor** spawns one `opencode run` per task in a working copy of the
content repo, which writes the page and pushes it back.

Today that pipeline is wired to the original single-org GitLab setup it was
built for. This spec removes those assumptions so the pipeline works for any
content repo on any host, and converges it with `content-source.yaml` so a
single config drives both reading (embed) and writing (ingest).

Decisions taken (see the design forks):

1. **Pluggable executor.** Define an `Executor` interface; `opencode` is the
   default implementation; the planner output (`Task`) is unchanged so other
   executors (direct SDK, CI, a mongoose skill) can drop in later.
2. **Working copy + branch derive from `content-source.yaml`.** Ingest writes
   to the same `git`/`submodule`/`local` source it embeds from; the push
   branch comes from `content.branch` (or `content.ref` if branch-like, else
   the remote default). `--workdir-kb` / `--branch` remain as overrides.
3. **Host + kind + category routing.** `host` (github|gitlab|local) says
   *where*; `type` is the enumeration *kind* (files|repos-in-group|pdfs|
   synthesised); pages route to a source purely by `target_category`
   (+ optional subcategory / id-prefix) with no hardcoded taxonomy.

## Current couplings (to remove)

| # | Coupling | Location |
|---|----------|----------|
| 1 | `git push origin master` hardcoded | `exec.go` per-page instruction |
| 2 | `pagePathForRepo` hardcodes `wiki/` | `plan.go` |
| 3 | `matchSource` special-cases `systems/backend` / `systems/frontend` | `plan.go` |
| 4 | `guessWorkdirKB` hardcodes `./kb`; `--workdir-kb` separate from content config | `cli/ingest.go` |
| 5 | gitlab-flavored `type` vocabulary (`gitlab` / `gitlab-group` / `pdf-corpus`) | `sources.yaml` schema |
| 6 | executor is opencode-only, flags inline in `runOne` | `exec.go` |

## Design

### Source schema (host + kind + routing)

```yaml
sources:
  - id: adr
    host: github          # where: github | gitlab | local
    type: files           # kind: files | repos-in-group | pdfs | synthesised
    repo: your-org/architecture
    enumerate:            # kind-specific modifiers
      glob: "**/*.md"
      path: adr/
    target_category: adr  # routing key (see below)
    template: adr.md
    prompt: prompts/adr.md
```

- `host` selects the fetch tooling the sub-agent uses (gh for github, glab for
  gitlab) and is surfaced in the prompt's `{{source}}` line.
- `type` is the enumeration kind. `enumerate` carries the modifiers
  (`glob`, `path`, `include_subgroups`). The legacy `gitlab`/`gitlab-group`
  values are accepted on read and normalized (`gitlab`→`files`+host gitlab,
  `gitlab-group`→`repos-in-group`+host gitlab, `pdf-corpus`→`pdfs`) so existing
  registries keep working.

### Routing (generic, no hardcoded taxonomy)

A page routes to exactly one source by `target_category`, resolved in order:

1. `category/subcategory` exact match (e.g. `operations/runbooks`).
2. `category` exact match (e.g. `adr`, `concepts`).
3. **Longest `target_category` that is a path-prefix of the page id** (e.g.
   page `systems/backend/foo` → source whose `target_category` is
   `systems/backend`). This replaces the hardcoded `systems/*` branch with a
   generic prefix rule that works for any nested taxonomy.

Unmatched pages are skipped (optionally surfaced as a warning).

### Working copy + branch (from content-source.yaml)

`ingest --execute` resolves its target from `content-source.yaml`:

- **working copy:** the resolved source path — the `local` path, the
  `submodule` path, or the `git` cache checkout (reusing
  `internal/contentsource`’s resolver). `--workdir-kb` overrides.
- **branch:** `content.branch` if set; else `content.ref` when it looks like a
  branch (not a tag/SHA); else the remote default (`git symbolic-ref
  refs/remotes/origin/HEAD`). `--branch` overrides.
- **wiki dir:** `content.layout.wiki` — drives `Task.PagePath`
  (`<wiki>/<id>.md`) so the sub-agent writes to the right place regardless of
  repo layout.

A new optional `content.branch` is added to the content-source schema for the
common case where you embed a pinned tag (`ref: v1.2.0`) but ingest should push
to a moving branch (`branch: main`).

### Executor interface

```go
// Executor runs a single ingestion Task and reports the outcome.
type Executor interface {
    Name() string
    Exec(ctx context.Context, t Task, env ExecEnv) Result
}

// ExecEnv is the resolved per-run environment shared by all tasks.
type ExecEnv struct {
    WorkdirKB string
    Branch    string
    Out, Err  io.Writer
}
```

- `agentCLIExecutor` is the spawn-an-agent-CLI implementation, configured per
  CLI: `opencode` (`opencode run --dir <wd>`) and `claude` (Claude Code,
  `claude -p` with cwd = working copy). `--executor opencode|claude` selects
  it; both share the per-page commit/push instruction parameterized by
  `env.Branch`. A direct-SDK / mongoose-skill executor can implement the same
  interface without the agent-CLI shape.
- `Run(ctx, tasks, opts)` builds the default executor when none is supplied,
  preserving the existing concurrency / consecutive-failure / skip-reviewed
  control loop (that loop is executor-agnostic).
- The commit/push step uses `git push origin <branch>` — no more `master`.

### CLI

`mk ingest [--execute]` unchanged in spirit; new behavior:

- With no `--workdir-kb`, resolve the working copy + branch + wiki dir from
  `content-source.yaml` (error clearly if `type: none`/absent and `--execute`
  is requested without `--workdir-kb`).
- `--branch` override added.
- Plan output (`Task` JSONL) is unchanged — the contract other consumers rely
  on stays stable, except `page_path` now honors the configured wiki dir.

## Security / safety

- Push target is an explicit branch (no implicit `master`); the executor still
  validates `page_path` stays within the working copy (existing
  `isPathWithinBase`).
- Private source fetch by sub-agents continues to use the host CLI's cached
  OAuth (gh/glab) — meerkat holds no tokens.
- Executor remains sandboxed to the user's own opencode session; per-task
  wall-clock cap unchanged.

## Migration

- Existing `sources.yaml` files keep working via the legacy-type
  normalization; new registries use `host` + kind.
- `git push origin master` consumers move to the derived/`--branch` value;
  default for an unconfigured branch is the remote default (commonly `main`).
- No change to the embedded-content / build flow (that is content-sources.md).

## Testing

- Routing: table tests for category, category/subcategory, and prefix matches
  (incl. the former `systems/backend` case) + legacy-type normalization.
- Branch derivation: branch-like ref vs tag vs explicit `content.branch` vs
  remote default (against a local fixture repo).
- Executor: a fake `Executor` drives `Run` to assert the control loop
  (parallelism cap, consecutive-failure stop, skip-reviewed) without spawning
  opencode; opencode instruction asserts it pushes to the resolved branch.

## Open / follow-ups

- **Direct-SDK / mongoose-skill executors** — the interface now has two
  agent-CLI implementations (opencode, claude); a headless in-process
  Anthropic-SDK executor (meerkat fetches sources + writes + pushes in Go,
  holds an API key) and a mongoose-skill executor remain as follow-ups.
- **Multi-host sub-agent fetch tooling** is the sub-agent's concern (gh/glab
  MCP tools); meerkat only conveys `host` in the prompt.
- **`mk ingest sources` / planner enrichment** generalization beyond category
  routing (e.g. service-catalog enrichment) stays deployment-specific.
