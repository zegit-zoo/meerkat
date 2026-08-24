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
3. The `release.yml` workflow triggers on the tag push, running two jobs
   in parallel once `verify` passes:
   - **`verify` job** — re-runs the full gate (lint + test + vuln + gosec + gitleaks) on the exact tagged SHA.
   - **`goreleaser` job** (runs only after `verify` passes) — builds cross-platform binaries (darwin/linux/windows, amd64/arm64), creates archives and a checksums file, generates SPDX SBOMs via syft, signs the checksums file with cosign (keyless, GitHub OIDC), and publishes a GitHub Release with all artifacts attached.
   - **`docker` job** (runs only after `verify` passes, in parallel with `goreleaser`) — builds the hardened multi-stage [`Dockerfile`](../Dockerfile) for `linux/amd64` + `linux/arm64` via QEMU + buildx, pushes the manifest list to `ghcr.io/zegit-zoo/meerkat`, signs it with cosign (keyless, GitHub OIDC), and attaches a Syft SBOM + SLSA provenance attestation via buildx's native `--sbom`/`--provenance`. See [docs/CONTAINER.md](CONTAINER.md).

The released artifacts are:
- `meerkat_<v>_<os>_<arch>.tar.gz` / `.zip` (Windows)
- `meerkat-bootstrap_<v>_<os>_<arch>` / `.exe` (Windows) — a standalone
  binary (not archived) that installs a verified `zegit-zoo/meerkat`
  release independently of whatever updater the binary at its
  `--destination` understands; see [docs/INSTALL.md's "Converging from a
  downstream fork"](INSTALL.md#converging-from-a-downstream-fork)
- `meerkat_<v>_checksums.txt` (SHA-256, covering every archive and
  standalone binary above)
- `meerkat_<v>_checksums.txt.sigstore.json` (cosign Sigstore bundle: signature + certificate + Rekor inclusion proof)
- `*.sbom.json` (SPDX SBOM per archive and per standalone binary)
- `ghcr.io/zegit-zoo/meerkat:X.Y.Z` / `:X.Y` / `:latest` (multi-arch OCI
  image, signed + attested. Note these image tags drop the git tag's
  leading `v` — a `vX.Y.Z` git tag publishes exactly these three
  aliases — see [docs/CONTAINER.md](CONTAINER.md))

> **Note:** the repository is public. Downloading release assets is
> anonymous — no token or `gh` login required. A token only helps if you
> hit GitHub's anonymous API rate limit. Pulling the container image from
> `ghcr.io` is likewise anonymous once the package is public.

## Tag protection

`v*` tags are governed by a repository ruleset with no bypass:

- signed tags only (`git tag -s`)
- name must match `vX.Y.Z` exactly
- no deletion, no moving a tag once pushed

The name pattern is load-bearing, not cosmetic: `CertIdentityRegexp` in
`internal/update/cosign.go` is anchored to
`refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$`, so a tag like `v1.0` or a
prerelease suffix would trigger the workflow and publish a release whose
signature `mk update` then refuses to verify. The ruleset rejects the tag
at push time instead. If a prerelease convention is ever wanted, both the
regexp and the ruleset pattern have to change together.

Immutability matters for the same reason: the workflow signs whatever the
tag points at, so a moved tag yields a validly signed release with
different bytes. Cut a new version instead.

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

## Verifying the container image (consumer side)

```sh
cosign verify \
  --certificate-identity-regexp '^https://github.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/zegit-zoo/meerkat:X.Y.Z
```

Note `X.Y.Z` here is the *image* tag (no leading `v`), even though the
`--certificate-identity-regexp` above still matches `refs/tags/vX.Y.Z` —
that regexp is anchored to the *source* git ref the workflow ran from,
which does keep the `v`. See [docs/CONTAINER.md](CONTAINER.md) for the
full tag list.

Same identity/issuer pair as the checksums-file verification above,
applied to the image manifest instead of a blob. `cosign verify` for an
OCI image queries the public Rekor transparency log directly (rather
than checking an embedded offline inclusion proof, which is what
`verify-blob --bundle` does). See [docs/CONTAINER.md](CONTAINER.md) for
running the image itself, including the read-only-filesystem flags and
the SBOM/provenance inspection commands.
