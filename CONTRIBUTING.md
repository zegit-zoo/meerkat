# Contributing to meerkat

meerkat is a single-binary Go CLI (`mk`) with a handful of local gates
that CI also enforces. Nothing here is optional — a PR that skips a
step below will fail CI. Run the gates locally first; it's faster than
the round-trip.

## Build and run

```bash
git clone https://github.com/zegit-zoo/meerkat.git
cd meerkat
make build       # -> bin/meerkat (+ bin/mk symlink)
./bin/mk version
```

`make build` depends on `make sync`, which populates the embedded
content dirs from `content-source.yaml`. There's no `content-source.yaml`
in this repo by default, so `sync` falls back to the committed empty
placeholders — that's fine for building and running the test suite. To
point the build at a real knowledge base, copy `content-source.example.yaml`
to `content-source.yaml` and see `docs/INSTALL.md`.

## One-time setup: install the hooks

This repo uses [pre-commit](https://pre-commit.com/) to run the same
checks locally that CI runs. Install **both** hook types — installing
only the first silently skips the push-time gates (tests, docs-check,
govulncheck):

```bash
brew install pre-commit                    # or: pipx install pre-commit
pre-commit install                         # gates `git commit`
pre-commit install --hook-type pre-push    # gates `git push`
```

What each hook stage runs:

- **commit-time**: `gitleaks` (secrets scan, per `.gitleaks.toml`);
  `golangci-lint` (per `.golangci.yml`: errcheck, govet, staticcheck,
  unused, gosec, misspell, unconvert, unparam, prealloc, whitespace,
  bodyclose, plus gofmt/goimports); a config-verify check that runs
  when `.golangci.yml` itself changes.
- **push-time**: `make docs-check` (generated docs in sync), `make test`
  (full suite, race detector), `make vuln` (govulncheck).

Note: `gosec` runs both inside `golangci-lint` (low-severity,
low-confidence — commit-time) and as its own high-severity-only target,
`make gosec`, used in the release gate (`make pre-release`).

Do not bypass these with `git commit --no-verify`, `git push --no-verify`,
or `SKIP=<hook>`. If a hook is wrong, fix the hook, don't bypass it — a
repo hook blocks commands containing `--no-verify` or `SKIP=` outright.

## What CI runs

Every push and PR runs these jobs. Run the equivalent `make` target
locally before you push:

| CI job | Local equivalent | What it checks |
|---|---|---|
| Lint | `make lint` | golangci-lint: govet, staticcheck, errcheck, gosec, unused, misspell, unconvert, unparam, prealloc, whitespace, bodyclose, gofmt/goimports |
| Lint → tidy check | `go mod tidy` (then `git diff --exit-code -- go.mod go.sum`) | `go.mod`/`go.sum` must already be tidy — drift fails CI |
| Lint → docs check | `make docs-check` | `docs/CLI.md` is **generated** from the cobra command tree; if it's stale, CI fails |
| Test | `make cover-check` | full test suite with `-race` (needs `CGO_ENABLED=1`), then fails if total coverage drops below the floor in `Makefile` (`COVERAGE_MIN`, currently `48`) |
| Vulnerability scan | `make vuln` | govulncheck against the actual import graph |
| gitleaks | `make gitleaks` | scans history + working tree for committed secrets, per `.gitleaks.toml` |

A convenience target runs the fast subset in one shot:

```bash
make pre-push   # lint + test + docs-check — same gate the pre-push hook runs
```

Security scans (`vuln`, `gosec`, `gitleaks`) run in CI as separate jobs
but aren't part of `pre-push` since they're slower; run them together
with `make pre-release` before tagging a release, or individually with
`make vuln` / `make gosec` / `make gitleaks`. See `docs/SECURITY.md` for
what each scanner catches and how to fix findings.

### If `docs/CLI.md` is out of sync

Any change to a cobra command (`internal/cli/*.go`) changes the
generated reference. Regenerate it and commit the result:

```bash
make docs
git add docs/CLI.md
```

### If coverage drops below the floor

`COVERAGE_MIN` in the `Makefile` is a ratchet — it only moves up. Add
tests to cover the new code rather than lowering the floor. If you
genuinely believe the floor should move, that's a discussion for the
PR, not a silent Makefile edit.

### If `go.mod`/`go.sum` are out of sync

Run `go mod tidy` and commit whatever it changes.

## Tests

```bash
make test         # go test -race -count=1 ./...
make test-cover    # same, plus a coverage summary
```

The race detector requires cgo: `CGO_ENABLED=1`. If tests pass locally
without it but fail in CI, that's the likely cause — CI sets it
explicitly for the test job.

## Commit messages: Conventional Commits

This repo follows [Conventional Commits](https://www.conventionalcommits.org/)
(`type(scope): summary`, e.g. `fix(cli): correct --json flag help text`).
This isn't a style preference — `.goreleaser.yaml` generates release
notes from the commit log and filters by these prefixes (`docs:`,
`test:`, and `chore:` commits are excluded from the changelog). A
commit that doesn't follow the convention either pollutes the release
notes or silently disappears from them.

Common types used in this repo: `feat`, `fix`, `docs`, `test`, `chore`,
`ci`. Scope is optional but encouraged for anything touching a specific
package or subsystem (e.g. `feat(ingest): ...`).

## Pull request flow

The default branch is `main`. (Some older docs in this repo mention
pushing directly to `master` — that's stale internal convention from
before the project moved to a PR-gated flow; ignore it.)

1. Fork the repo.
2. Branch from `main`.
3. Make your change, with tests.
4. Confirm the local gates pass: `make pre-push` (and `make pre-release`
   if you touched anything security-sensitive).
5. If you changed CLI commands, run `make docs` and commit the updated
   `docs/CLI.md`.
6. If you changed dependencies, run `go mod tidy` and commit the result.
7. Open a PR against `main`. Use Conventional Commits for your commit
   messages (see above) — squash-worthy commit history helps but isn't
   required; the PR title/commits feed the changelog.

CI must pass (lint, test+coverage, vuln, gitleaks) before a PR is
merged.

## Branch and tag protection

`main` is protected by a repository ruleset with no bypass, so it applies
to maintainers too:

- **Commits must be signed.** Set signing up before your first push —
  `git config gpg.format ssh`, `git config user.signingkey <your-key.pub>`,
  `git config commit.gpgsign true`. An unsigned commit is rejected at push
  time, not at review time.
- **No force-push, no branch deletion.** History on `main` is append-only.

Release tags (`v*`) are protected separately: they must be signed, must
match `vX.Y.Z` exactly, and once pushed can never be deleted or moved.

That last rule is not bureaucracy. The release workflow signs whatever a
`v*.*.*` tag points at, using the repository's own OIDC identity, and
`mk update` verifies that signature against a certificate identity regexp
pinned to `refs/tags/v<major>.<minor>.<patch>`. A tag shaped like `v1.0`
or `v1.0.0-rc1` would therefore produce a release whose signature
`mk update` refuses; the pattern rule rejects it at push time instead.
And since moving a tag would yield a *validly signed* release with
different bytes, immutability is what makes a pinned version mean
anything.

## Repo layout

See the "Repo layout" section in `README.md` for a package-by-package
map of `cmd/` and `internal/`.
