# AGENTS.md — how agents and humans work in this repo

Humans and their agents work here in parallel. This file is the contract. Read it fully before
touching anything. Repo-specific notes are in section 6; everything else is the primitive-engineering
baseline from [zegit-zoo/template-project](https://github.com/zegit-zoo/template-project).
`CONTRIBUTING.md` is the human-facing version of the same gates and is the source of truth where the
two differ.

## 1. First steps for any agent session

1. `gh issue list --label in-progress` — see what is already claimed. For mob work the issues live in
   the private design repo (section 6), so check there too.
2. Pick an **unclaimed** issue. Claim it *before* writing code:
   `gh issue edit <n> --add-assignee @me --add-label in-progress` and one comment:
   `Claimed by <agent> for <human>, <date>. Branch <branch>. ETA <when>.`
3. Never work on an issue that is assigned to someone else or carries `in-progress`.
   If you must touch it, comment first and wait for a reply.
4. Work on a branch named `<owner>/<issue>-<slug>` (`claude/12-tree-manifest`, `jonas/3-s3-backend`).
   One issue per branch, one PR per issue; never stack PRs. **Never push the default branch.**
5. Run `pre-commit install` and `pre-commit install --hook-type pre-push` once per clone. Commit
   and push hooks are mandatory; hook-bypass flags are forbidden.
6. Open the PR as a draft, link the issue, switch it to ready when CI is green,
   replace `in-progress` with `needs-review`, and unassign yourself. Someone else merges.
7. Blocked? Add `blocked`, say what unblocks it in a comment, unassign, move on.

Labels every repo carries: `in-progress`, `needs-review`, `blocked` (plus GitHub's defaults).

## 2. Communication

- Decisions and questions go in **issue comments**, not chat. Quote the issue number in commits
  (`feat(collections): lazy mount on first traversal (meerkat-mob#5)`).
- Design notes longer than a comment go in `docs/design/` (public) or the private design repo
  (section 6), with a link from the issue.
- When you change a shared type, interface or schema, comment on every open issue that builds on it.
- Do not edit another party's open PR; comment on it.
- Every PR body carries an "Assumptions & Decisions" block: what was decided on the reviewer's
  behalf, so review can reverse it in one place.

## 3. Code conventions (Go repos)

- Go, single module, the version pinned in `go.mod`, CGO off for the binary (`-race` tests need
  `CGO_ENABLED=1`), one static binary under `cmd/meerkat`; packages under `internal/`.
- Conventional commits; subject ≤ 72 chars; body says why. `.goreleaser.yaml` builds release
  notes from the prefixes.
- Tests colocated (`*_test.go`); `make test` runs with `-race`; coverage floor `COVERAGE_MIN` in
  the Makefile only moves up.
- Lint is golangci-lint v2 with `.golangci.yml`; `make lint` must be clean; no new exclusions
  without a comment saying why.
- Secrets never enter the repo; gitleaks runs on every commit and in CI. Credentials, HMAC keys
  and tokens come from the environment or the provider's default chain, never from config.
- Errors are returned, not printed, until `main`; cobra has `SilenceErrors` so nothing prints twice.
- The disclosure rule is an invariant, not a preference: no query text, page ID, collection
  name, bucket, endpoint or session ID on a span or a metric label. A test enforces it.

## 4. Docs conventions (all repos)

- Markdown, one topic per file, frontmatter only where a consumer needs it (meerkat/OKF pages).
- Filenames kebab-case; dates ISO (2026-09-17); decisions carry who and when.
- `markdownlint` runs on commit and in CI (`.markdownlint.yaml`; fixtures excluded in
  `.markdownlintignore`). `docs/CLI.md` is generated: run `make docs`, never edit it.
- Large files (audio, video, binaries) never enter git.

## 5. Local gotchas

- `main` requires **signed commits** (repository ruleset, no bypass); a squash-merge does not
  sign for you. Set up SSH or GPG signing before the first push.
- The host Go may be newer than `go.mod`; pin with `GOTOOLCHAIN=go<version>` from `go.mod`
  before running the gates.
- The GitHub gitleaks Action needs a paid org license; CI runs the gitleaks CLI instead.
- `golangci-lint` runs collide when two worktrees lint at once; serialize them.
- A credential-helper test prompts when git can reach a terminal; run tests with
  `GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=/bin/false SSH_ASKPASS=/bin/false`.
- The S3 conformance job needs Garage and Versity Gateway; locally use `scripts/garage-up.sh`
  and `scripts/versitygw-up.sh` and set `MEERKAT_TEST_S3_ENDPOINT`. MinIO left the matrix on
  2026-09-25 when its public container images were withdrawn.

## 6. Repo-specific: meerkat

Public code repo for meerkat, the vigilant guard and informer of the zegit platform: a
single-binary knowledge-base server with CLI, MCP and HTTP surfaces (`README.md`). Maintainer:
Jonas. The **meerkat mob** programme (tree of knowledge bases in S3, hot/cold paths, retrieval
SLIs, self-improving intake) is designed and tracked in the private repo
[zegit-zoo/meerkat-mob](https://github.com/zegit-zoo/meerkat-mob).

- **Requirements are the contract.** The baseline lives in `meerkat-mob/requirements/`
  (`MK-<AREA>-<NN>` IDs, MoSCoW priority, milestone, source, status, delivered-by). A PR that
  implements or changes a requirement cites its ID in the body and, when it changes scope,
  records the decision in `meerkat-mob/requirements/decisions.md` with who and when.
- **Design docs are public, issues are private.** Architecture for a delivered feature lives in
  `docs/design/*.md` here; the reasoning, review decisions and issue bodies live in meerkat-mob
  because GitHub has no private issues on public repos. Copy what graduates by hand; never move
  private content here.
- **Gates.** `make lint`, `make cover-check`, `make vuln`, `make gosec`, `make gitleaks`,
  `make docs-check`, and `markdownlint` all run in CI; the pre-commit hooks run the same set at
  commit and push time. Run them before opening a PR.
- **Shared S3 state is single-writer-per-key.** Garage ignores `If-None-Match: *` and `If-Match`
  on PUT; anything that needs multi-writer safety on one key must run on AWS, Versity Gateway
  or GCS, and
  the startup probe refuses otherwise unless `single_writer: true` is declared.
- **Never quote a vendor's authority when the vendor exposes it as a tool.** Vendor knowledge
  becomes a pointer page to the vendor's MCP server, not copied documentation.
