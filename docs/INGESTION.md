# Ingestion pipeline

How placeholder pages in the KB get turned into useful content.

## TL;DR

Meerkat is a **planner**. OpenCode (running `gpt-5.5-fast` by
default) is the **executor**. The Go binary ships with no LLM
credentials.

```
sources.yaml + prompts + templates
        │
        ▼
 mk ingest (planner) ──► JSONL batch
                              │
                              ▼
 mk ingest --execute (runner) ──► spawns one
                                  `opencode run --model gpt-5.5-fast`
                                  per page; 5-min wall-clock cap
                                  │
                                  ▼
                            writes wiki/<page>.md
                            commits + pushes the branch resolved
                            from content-source.yaml
```

## Source registry — `<your-content-repo>/ingestion/sources.yaml`

The single declarative source-of-truth that the planner, the
bootstrap script, and `mk ingest sources` all read.

```yaml
sources:
  - id: <unique slug>
    type: gitlab | gitlab-group | pdf-corpus | synthesised
    repo:  ...                   # for type=gitlab
    group: ...                   # for type=gitlab-group
    path:  ...                   # for type=pdf-corpus
    target_category: <category>  # destination under wiki/
    enumerate:
      kind: files | repos-in-group | pdfs
      glob: "*.md"               # optional file filter
      path: subdir               # optional repo subdir filter
      include_subgroups: true    # for gitlab-group only
    template: <name>.md          # under templates/
    prompt: prompts/<name>.md    # under ingestion/prompts/
    schedule: weekly | monthly | on-change
    enrichment: [past-incidents, datadog-service-catalog]   # optional
    model: openai/gpt-5.5-fast   # optional per-source override
```

Run `mk ingest sources` to print the live registry.

## Example source registry

A real registry can have any number of sources. Here's a small
illustrative set showing the three common shapes — a GitLab repo, a
PDF corpus, and a synthesised category:

| ID | Type | Source | Target |
|----|------|--------|--------|
| `adr` | gitlab | `your-org/architecture/architecture-decision-record` (under `architecture-decision-records/`) | `adr/` |
| `policies` | pdf-corpus | `<your-extracted-docs>/*.pdf` | `policies/` |
| `concepts` | synthesised | seed list, cross-links from everything else | `concepts/` |

## Per-source prompts

`<your-content-repo>/ingestion/prompts/*.md`. Each one is a system-prompt
markdown file with `{{var}}` placeholders the planner substitutes:

| Variable | Filled with |
|----------|-------------|
| `{{page_id}}` | `policies/incident-response` |
| `{{page_path}}` | `wiki/policies/incident-response.md` |
| `{{page_id_basename}}` | `incident-response` |
| `{{template_path}}` | `templates/policy.md` |
| `{{source}}` | one-line summary, e.g. `type=gitlab repo=… enrichment=…` |
| `{{enrichment}}` | comma-separated, e.g. `datadog-service-catalog,past-incidents` |
| `{{now}}` | current ISO-8601 UTC time |

Prompts live in the **kb repo** (not the CLI repo) so prompt
adjustments don't require a meerkat release.

## Planner (`mk ingest`)

```bash
mk ingest                            # plan-only, all stale pages, JSONL on stdout
mk ingest --source policies          # plan one source
mk ingest --page concepts/Foo        # plan one page
mk ingest --status placeholder       # narrow by status
mk ingest --batch-file batch.jsonl   # plan to file
mk ingest --dry-run                  # show plan, do not execute
mk ingest sources                    # list embedded source registry
```

The planner walks the embedded KB pages, applies filters, matches
each to its source via `category` (and `subcategory` for `operations/*`),
renders the prompt with substitutions, and emits one JSONL `Task`
per page:

```json
{
  "page_id": "policies/foo",
  "page_path": "wiki/policies/foo.md",
  "source_id": "policies",
  "source": { ... },
  "prompt": "<full rendered system prompt>",
  "model": "openai/gpt-5.5-fast",
  "subagent_type": "general",
  "wall_clock_cap_seconds": 300
}
```

JSONL is the contract: anything that consumes `mk ingest` output
is just iterating over it.

### OKF bundles are never selected

The planner's default `--status` filter is `placeholder` /
`ingest-failed` — meerkat's own "needs work" vocabulary. A KB that mixes
in an [OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
bundle (see [OKF.md](OKF.md)) uses OKF's own `status` values
(`draft`/`stable`/`deprecated`) on those pages instead, untranslated —
none of which match the default filter. A plain `mk ingest` or `mk
ingest --execute` therefore never selects an OKF-authored page, with or
without `--source`/`--page` narrowing further; the only way to target
one is an explicit `mk ingest --status draft` (or whichever OKF value)
naming it directly.

## Executor (`mk ingest --execute`)

```bash
mk ingest --execute                                    # all stale, parallel=1
mk ingest --source policies --execute --max-parallel 4 # parallel=4
mk ingest --execute --workdir-kb ~/your-kb-repo
```

For each task, the executor spawns one `opencode run` subprocess:

```
opencode run \
  --model openai/gpt-5.5-fast \
  --dir ${WORKDIR_KB} \
  --title "meerkat: <page-id>" \
  "<wrapped instruction>"
```

By default, permission prompts stay **on** — the sub-agent asks
before acting, the same as an interactive session. Add
`--trust-sources` to run unattended (e.g. from CI) with prompts
disabled (`--dangerously-skip-permissions` passed to the agent CLI):

```bash
mk ingest --execute --trust-sources
```

> **Risk:** `Task.Prompt` is rendered from `ingestion/prompts/*.md` in
> the ingested content source. With `--trust-sources`, a malicious or
> compromised prompt file in *any* source repo listed in
> `sources.yaml` can make the sub-agent take arbitrary action —
> including in the working copy that holds your push credentials —
> with no permission prompt in the way. Only pass `--trust-sources`
> if you trust every source repo in `sources.yaml` as much as code
> you'd merge unreviewed. Meerkat prints a stderr warning before the
> first agent spawns when this flag is set.

The wrapped instruction prepends a small "do exactly this one
page, commit + push, then stop" header so the sub-agent stays
focused. Each session writes the page, runs `git pull --rebase`,
commits, and pushes to the branch resolved from `content-source.yaml`
(`content.branch`, or the remote default — commonly `main`).

Concurrency is bounded by `--max-parallel` via a counting
semaphore. Each goroutine acquires the semaphore, spawns its
subprocess, releases on return.

### Skip-if-already-reviewed

Before spawning, the executor reads the page's `status:` from disk.
If it's already `reviewed` (e.g. another worker raced and finished
it), the page is skipped. This makes re-running ingest cheap.

### Wall-clock cap

Per-task default is 5 minutes. `context.WithTimeout` propagates
into the subprocess, which gets killed if it exceeds the cap. The
result line is recorded as `timeout` and processing continues.

### Stop-on-consecutive-failures

`--max-consecutive-failures N` halts the executor after N tasks in
a row fail (default 0 = never auto-stop). Useful as a guardrail
when the upstream model API is flaking.

## Triggers

- **Manual** — `mk ingest --page <id>` or `--source <id>` from a
  developer's shell.
- **Scheduled** — A scheduled CI job in your kb repo (GitHub Actions,
  GitLab CI, cron, etc.) runs `mk ingest --execute` nightly. Stale
  pages get refreshed.
- **On push** — Each upstream source (e.g. `your-org/architecture`,
  `your-org/rfc`, etc.) can carry a tiny CI job that triggers a
  downstream `mk ingest --source <id>` in your kb repo via a
  cross-repository pipeline/workflow trigger.

## Auth

The Go binary holds **zero** LLM credentials. `opencode` uses the
user's own ChatGPT/OpenAI account (signed in via OAuth or API key
in their `~/.local/share/opencode/auth.json`).

For host API calls inside sub-agents (e.g. fetching a source file),
OpenCode's bundled MCP tools reuse the host CLI's own OAuth cache:
`glab` for GitLab sources, `gh` for GitHub sources. This is the
sub-agent's own tooling, separate from meerkat's `internal/auth`
`TokenProvider` (which `mk update` and the `content-source.yaml` git
sync use, and which only borrows a `gh` — i.e. GitHub — token).

## Quality bar

Every prompt enforces:

- **Faithful to source** — never invent facts. If a section has
  no source, write `_Unknown — populate from <hint>._`.
- **Preserve frontmatter** — only `status`, `last_ingested`, and
  enrichment-derived fields change. `id`, `title`, `category`,
  `source.*` are fixed at bootstrap time.
- **Cross-links via `related:`** — pages link to other categories
  by ID; this is what makes `concepts/` synthesisable.
- **One commit per page** — small atomic commits, easy to revert
  individually if a page comes out wrong.

## Watching progress

```bash
~/your-kb-repo/ingestion/status.sh           # one-shot
watch -n 5 ~/your-kb-repo/ingestion/status.sh # live
tail -f /tmp/meerkat-mk-<source>.out          # per-batch driver log
cd ~/your-kb-repo && watch -n 10 'git pull --quiet && git log --oneline v0.1.0-skeleton..HEAD | head -20'
```

## Failure modes

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| `mk ingest --execute` hangs on first page | `opencode` not on PATH | `brew install opencode` or fix `$PATH` |
| Task exits `failed` immediately | `opencode auth` not set | `opencode auth login` |
| Task `status: ingest-failed` w/ "Task tool used" | Sub-agent delegated to its own Task tool, exiting before doing the work | Prompt rewords the "DO NOT use the Task tool" line — already enforced for current prompts |
| 3 in a row fail | Likely upstream API flake or rate-limit | `mk ingest --execute --max-consecutive-failures 3` is the gate |
| Pages that look truncated | Hit per-task wall clock | bump `--wall-clock-cap` (default 300s) for that source's run |

## Why not embed the LLM call in the binary

We discussed this. Trade-offs:

| Option | Pros | Cons |
|--------|------|------|
| Shell out to `opencode run` (chosen) | No LLM client in binary; users own their model + provider; sub-agent isolation | `opencode` is a hard prereq |
| Direct OpenAI SDK call from Go | Deterministic, scriptable from CI | Binary must hold provider creds; we'd reimplement tool loop, MCP wiring, retries |
| OpenCode MCP server tool (`mk_ingest_page` invoked from inside an OpenCode session) | Free `task`-tool isolation; declarative model in skill frontmatter | Not automatable from cron without a driver session |

We kept the planner pure and made the executor swap-able: today
it's option A; switching to C is a 50-line change in `internal/ingest/exec.go`.
