# Security

Meerkat ships under a layered security suite that runs on every push
and gates every release. This doc explains what runs, what each tool
catches, and how to fix the things they flag.

---

## What runs

| Tool | What it catches | Where | Severity gate |
|------|-----------------|-------|----------------|
| `govulncheck` | Known CVEs in our **actual import graph** (Go vuln DB) | CI (Vulnerability scan job) + `make vuln` | Hard fail on any reachable vuln |
| `gosec` | Go-specific weaknesses (weak crypto, command injection, file traversal, hardcoded creds) | CI (gosec job, and low-severity inside golangci-lint) + release gate + `make gosec` | Hard fail on HIGH severity, medium confidence |
| `gitleaks` | Accidentally committed secrets (PATs, OAuth tokens, private keys) | CI (gitleaks job) + `make gitleaks` | Hard fail on any leak |
| `goreleaser sboms` | SPDX SBOM generated per release artifact via syft | CI release workflow | Attached to the GitHub release |
| `goreleaser signs` | Cosign keyless (Fulcio + Rekor) signature on the checksums file | CI release workflow | Cosign-verifiable transparency-logged signature |
| `docker buildx --sbom` | SPDX SBOM attestation for the container image (BuildKit's syft-based scanner) | CI release workflow (`docker` job) | Attached to the image manifest as an OCI referrer |
| `docker buildx --provenance` | SLSA provenance attestation for the container image | CI release workflow (`docker` job) | Attached to the image manifest as an OCI referrer |
| cosign (image) | Cosign keyless (Fulcio + Rekor) signature on the pushed image manifest | CI release workflow (`docker` job) | Cosign-verifiable transparency-logged signature |

CI (GitHub Actions) runs lint, test, govulncheck, gosec, and gitleaks as
**parallel** jobs on every push and PR; `release.yml` re-runs the full
gate before GoReleaser, so a vulnerable `master` cannot be tagged.

We don't run `semgrep-sast` (~19 min on a fresh runner); `gosec` — its
own CI job at HIGH severity, plus the low-severity pass inside
golangci-lint — already covers the Go-specific weaknesses it would flag.

---

## Run locally

```bash
make security        # all three at once
make vuln            # govulncheck
make gosec           # gosec
make gitleaks        # gitleaks
```

Each target self-installs the tool from a pinned version (see the
`*_VERSION` block in `Makefile`) so devs don't need a separate
setup step. The install lands in a repo-local, version-named
directory — `.tools/<name>@<version>/<name>` — and the target runs
it from there by absolute path. **`$PATH` is never consulted**, so a
Homebrew `gosec` or `gitleaks` earlier on your `$PATH` cannot quietly
stand in for the pinned one and make your results differ from CI's;
the version-named directory is also the stamp, since `GOBIN` pointed
at it for exactly that `go install`. `govulncheck` is pinned the same
way — its vulnerability *database* is still fetched live on every run,
so the pin costs no freshness.

`make lint` installs the pinned `golangci-lint` into `.tools/` the same
way, so the linter that runs on your machine, in the pre-commit hook and
in CI is one pinned binary instead of three separately-named ones that
could drift apart.

`.tools/` is gitignored and excluded from the `gosec` walk. `make clean`
deliberately leaves it in place (the tool builds are expensive to redo
on every clean); `make clean-tools` removes it.

---

## Fixing findings

### `govulncheck` — vulnerable dependency

```text
Vulnerability #1: GO-2024-XXXX
  Module: golang.org/x/foo
    Found in: golang.org/x/foo@v0.5.0
    Fixed in: golang.org/x/foo@v0.6.1
    More info: https://pkg.go.dev/vuln/GO-2024-XXXX
```

Bump the dependency:

```bash
go get golang.org/x/foo@v0.6.1
go mod tidy
make vuln  # re-run to confirm
```

If the vuln is in a transitive dep we don't directly use, govulncheck
won't flag it (that's the point — it walks the actual call graph,
not just the lockfile). If you see one anyway, the call path it
shows is real.

### `gosec` — weakness in our code

| Rule | What | Typical fix |
|------|------|-------------|
| G101 | Hardcoded credentials | Move to env var; use `MEERKAT_API_KEY` pattern |
| G104 | Unhandled error | Wrap with `if err := ...; err != nil { return err }` or document the ignore with `// nolint:errcheck` and a reason |
| G107 | URL provided to HTTP request as taint source | Validate or allowlist the URL before the call |
| G201/G202 | SQL string formatting | We don't have SQL today; if added, use `database/sql` parameterised queries |
| G401-G403 | Weak crypto | Use `crypto/rand`, AES-256-GCM, ed25519, ChaCha20-Poly1305 |
| G601 | Implicit memory aliasing in for loop | Capture loop var into local before goroutine/closure |

If a finding is a known false positive, suppress with `-exclude=GNNN`
in the Makefile and document why in a comment alongside.

### `gitleaks` — committed secret

If the leak is in your working tree, **delete it and re-commit**.
If it's in history, you must **rotate the secret** first (assume
it's exposed) and then optionally rewrite history with
`git filter-repo` (coordinate the force-push).

Patterns we treat as not-secrets are listed in `.gitleaks.toml`'s
allowlist; add narrow entries there before loosening the rules.

### `goreleaser sboms` / `signs` failures

These run at release time only. Failures mean a release won't be
cut. Most common causes:

- `cosign` not in PATH on the runner — fix the base image.
- Missing `id-token: write` permission — Fulcio keyless signing needs
  OIDC. The CI snippet sets it; if you copy-paste a release job,
   preserve the permissions block.

---

## Audited and accepted findings

These items are regularly flagged by generic scanners but are
intentional in meerkat's design.

- `internal/update/install.go` (`#nosec G702`):
  `syscall.Exec(currentExe, ...)` is an intentional self-reexec after
  atomic binary swap. `currentExe` comes from `os.Executable()` and is
  resolved via `filepath.EvalSymlinks`.
- `internal/update/notify.go` (`#nosec G118`):
  background goroutine intentionally outlives the command to persist
  update-check cache.
- `internal/update/download.go` / `internal/update/cosign.go` (`#nosec G304`):
  local file paths are tempfiles created by meerkat itself and consumed
  in the same execution flow.
- External `cosign` invocation in `internal/update/cosign.go`:
  meerkat uses `exec.CommandContext` with explicit argv (no shell),
  verifies Fulcio certificate identity and OIDC issuer, and requires
  Rekor transparency-log verification.

Additional hardening in place:

- Update HTTP client refuses redirects outside `github.com`,
  `*.github.com`, and `*.githubusercontent.com` for token-bearing
  release/download requests.
- Ingest executor validates that task `page_path` resolves within the
  configured KB workdir before reading/writing page files, and `Finalize`
  (`internal/ingest/roles.go`) does the candidate read and both candidate
  writes through an `os.Root` opened on that workdir, addressed by the
  relative page path. The lexical check is a cheap pre-filter only: it
  compares `filepath.Abs`/`Rel` results and so cannot see symlinks, and the
  agent run whose results are being finalized is exactly who could plant
  one. `os.Root` re-resolves every component against the open directory and
  refuses to leave it, so a link inside the working copy that points outside
  is refused by the kernel rather than trusted by a string comparison
  (`TestFinalize_SymlinkOutOfWorkingCopyIsRefused`). The librarian's
  rewrite path (`Snapshot` and `FinalizeRewrites` in
  `internal/ingest/rewrite.go`) does the same (#91), and goes one step
  further, because a rewrite edits a page that already exists. A page the
  run replaced with a symlink (even one that stays inside the tree) or
  with a hard link to another file is rejected rather than read. The
  snapshot is restored by unlinking whatever is at the path and writing a
  fresh file, never by writing through a link
  (`TestFinalizeRewrites_ReplacedPageIsRejectedNotWrittenThrough`). The
  planning half goes through an `os.Root` too (#92):
  - The researcher's copy of each raw deposit, the validator's seed and
    read of each candidate, the rewrite planner's page check, and the
    tool-description proposal all go through a root opened once per plan.
  - A link left in a persistent working copy by an earlier run cannot
    redirect a planning write or read out of the tree.
  - A link sitting at a write's own path is unlinked and replaced, never
    written through.
  - See `internal/ingest/plan_root_test.go`.
- `type: url` / `type: gcs` / `type: s3` content archive extraction (`internal/contentsource/archive.go`)
  treats every entry as hostile: symlink and hardlink entries are skipped
  outright (never created, never followed — the same escape vector
  `internal/kbdir`'s read-side adapter is hardened against, reproduced here
  on the write side); an entry name that's absolute, traverses (`..`), or
  contains a backslash/colon (Windows drive-absolute confusion) is
  rejected outright rather than relying on containment alone; every write
  goes through an `os.Root` rooted at the extraction directory, so
  containment holds even against a name engineered to defeat a
  string-only check; and per-file, cumulative, and entry-count caps bound
  decompression against a zip-bomb-style archive. A `type: gcs` or
  `type: s3` bundle is extracted by that same code; a prefix mount on
  either applies the same entry-name validation and `os.Root` containment
  to remote object names, with per-file, cumulative and object-count caps
  of its own — one shared implementation for both providers
  (`internal/contentsource/objectstore.go`), so neither backend has a
  fetch path of its own to harden separately.

---

## Threat model

The threat model lives in [THREAT-MODEL.md](THREAT-MODEL.md). It covers the system and trust
boundaries, a data-flow diagram, the assets and their classification, the disclosure rule, the
threats and controls per surface, the design notes, and the residual risks.

---

## Reporting a vulnerability

See the [security policy](../SECURITY.md). It names the contact
(`security@primitive-engineering.se`, or a private advisory on this
repository), the response timeline, coordinated disclosure, safe harbour
and the supported versions.
