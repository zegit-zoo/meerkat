# Release process

## CI pipeline

Every push to `main` and every pull request (`.github/workflows/ci.yml`)
runs four independent jobs in parallel — there's no job-to-job dependency:

| Job | What it checks |
|-----|----------------|
| `lint` | `go vet`, `gofmt`, `go mod tidy` drift, `docs/CLI.md` sync |
| `test` | `go test -race` + coverage report (`coverage.out`) |
| `vuln` | `govulncheck ./...` — known CVEs in the import graph |
| `gitleaks` | Full-history scan for committed secrets |

`gosec` (static security analysis, HIGH severity gate) isn't part of this
flat CI run — it's part of the tag-triggered release gate below.

## Release flow

1. Ensure the working tree is clean and tests pass locally (`make pre-release`).
2. Tag the release commit:
   ```
   git tag -s v1.2.3 -m "release v1.2.3"
   git push origin v1.2.3
   ```
3. The `release.yml` workflow triggers on the tag push:
   - **`verify` job** — re-runs the full gate (lint + test + vuln + gosec + gitleaks) on the exact tagged SHA.
   - **`goreleaser` job** (runs only after `verify` passes) — builds cross-platform binaries (darwin/linux/windows, amd64/arm64), creates archives and a checksums file, generates SPDX SBOMs via syft, signs the checksums file with cosign (keyless, GitHub OIDC), and publishes a GitHub Release with all artifacts attached.

The released artifacts are:
- `meerkat_<v>_<os>_<arch>.tar.gz` / `.zip` (Windows)
- `meerkat_<v>_checksums.txt` (SHA-256)
- `meerkat_<v>_checksums.txt.sigstore.json` (cosign Sigstore bundle: signature + certificate + Rekor inclusion proof)
- `*.sbom.json` (SPDX SBOM per archive)

> **Note:** the repository is public. Downloading release assets is
> anonymous — no token or `gh` login required. A token only helps if you
> hit GitHub's anonymous API rate limit.

## Verifying a release (consumer side)

Download the checksums file and its cosign Sigstore bundle, then run:

```sh
cosign verify-blob \
  --certificate-identity-regexp '^https://github.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  --bundle meerkat_<v>_checksums.txt.sigstore.json \
  meerkat_<v>_checksums.txt
```

A successful verification confirms the checksums file was produced by the
`release.yml` workflow in this repository at the exact tagged ref, and that
the signature is included in the public Rekor transparency log — the
inclusion proof is embedded in the bundle, so this check happens offline
rather than by querying Rekor live.
