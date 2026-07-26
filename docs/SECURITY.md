# Security

Meerkat ships under a layered security suite that runs on every push
and gates every release. This doc explains what runs, what each tool
catches, and how to fix the things they flag.

---

## What runs

| Tool | What it catches | Where | Severity gate |
|------|-----------------|-------|----------------|
| `govulncheck` | Known CVEs in our **actual import graph** (Go vuln DB) | CI (Vulnerability scan job) + `make vuln` | Hard fail on any reachable vuln |
| `gosec` | Go-specific weaknesses (weak crypto, command injection, file traversal, hardcoded creds) | CI (inside golangci-lint) + `make gosec` | Hard fail on HIGH severity, medium confidence |
| `gitleaks` | Accidentally committed secrets (PATs, OAuth tokens, private keys) | CI (gitleaks job) + `make gitleaks` | Hard fail on any leak |
| `goreleaser sboms` | SPDX SBOM generated per release artifact via syft | CI release workflow | Attached to the GitHub release |
| `goreleaser signs` | Cosign keyless (Fulcio + Rekor) signature on the checksums file | CI release workflow | Cosign-verifiable transparency-logged signature |

CI (GitHub Actions) runs lint, test, govulncheck, and gitleaks as
**parallel** jobs on every push and PR; `release.yml` re-runs the full
gate before GoReleaser, so a vulnerable `main` cannot be tagged.

We don't run `semgrep-sast` (~19 min on a fresh runner); `gosec` (run
inside golangci-lint) already covers the Go-specific weaknesses it
would flag.

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
setup step. Pinned versions keep results reproducible across the
team.

---

## Fixing findings

### `govulncheck` — vulnerable dependency

```
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
  configured KB workdir before reading/writing page files.

---

## Threat model (sketch)

Meerkat is **not** read-only: `mk ingest --execute` writes pages to a
working copy, commits, and pushes them upstream. The threat surface
is small but worth being explicit about:

| Asset | Threat | Mitigation |
|-------|--------|-----------|
| Embedded wiki content | Tampering between source and binary | Content is embedded at build time; `mk version` records the content commit it was built from. The release binary's SHA256 is published and cosign-signed, so consumers can verify the exact bytes. |
| User's GitHub token (used by `mk update` and `mk ingest` git auth) | Disclosure via argv, on-disk config, or logging | The token (from the `gh` auth cache) is handed to the clone/fetch subprocess only via a `credential.helper` script that reads it back from an env var (`MEERKAT_GIT_TOKEN`) at request time — it never appears in argv, in the persisted remote URL, or in `.git/config`. The remote URL is scrubbed back to its tokenless form in a `defer` immediately after clone/fetch, including on error paths, so a live credential doesn't linger in the cache dir. |
| Downloaded release binary (in `mk update` flow) | Supply-chain swap | SHA256 verified against published `checksums.txt`; cosign signature on the checksums file (Rekor-logged); staged in a user-owned temp dir before final copy/move; `.old` backup during swap |
| `mk http serve` API key | Disclosure | `subtle.ConstantTimeCompare`; key never echoed; required (no anonymous mode) |
| `mk ingest --execute` spawns an agent CLI (`opencode` or `claude`) | Prompt injection: `Task.Prompt` is rendered from `ingestion/prompts/*.md` in the ingested content source, so a malicious prompt file in any source repo in `sources.yaml` is an arbitrary-action path, running with `cmd.Dir`/`--dir` set to a working copy that holds push credentials, at the operator's full privilege. The generated instruction itself includes a `git push` recipe. | Ingested content is treated as **trusted input** to the agent — meerkat does not sandbox or vet it. The real control is permission prompts: by default the agent CLI runs *with* its normal permission prompts, so an injected instruction still has to get past those before it acts. `--trust-sources` disables the prompts (passes `--dangerously-skip-permissions` to the agent CLI) for unattended/CI runs; it prints a stderr warning before executing. Operators who enable `--trust-sources` must trust every source repo listed in `sources.yaml` — as much as they trust code they'd merge unreviewed. |
| Templates / prompts / sources.yaml | Tampering at build time | Embedded at build time; `make security` includes them in the gosec walk |

---

## Reporting a vulnerability

For a security issue you'd rather not file as a public issue,
open a private security advisory on the project's GitHub repository
(Security → Advisories → "Report a vulnerability"). This keeps the
report confidential until a fix is published. We aim to respond
within 7 days and cut a `vX.Y.Z` patch release once the fix lands.
