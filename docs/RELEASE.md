# Release process

## CI pipeline

Every push to `main` and every pull request runs the repo's gates as
independent jobs in parallel — there's no job-to-job dependency, so one
failure doesn't mask the rest. Which jobs those are changes as gates are
added, so this page deliberately doesn't list them:
[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) is the source of
truth, the README's ["Build / test /
release"](../README.md#build--test--release) section names the `make` target
that runs each check locally, and [SECURITY.md](SECURITY.md) explains what
the security scanners catch.

The tag-triggered release workflow below re-runs the **full** gate —
including `gosec`, the HIGH-severity static-analysis pass — against the
exact tagged SHA before anything is published.

## Release flow

1. Ensure the working tree is clean and tests pass locally (`make pre-release`).
2. Tag the release commit:

   ```sh
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

## Homebrew tap bump (after the release publishes)

`brew install zegit-zoo/tap/meerkat` is served by a separate repository,
[`zegit-zoo/homebrew-tap`](https://github.com/zegit-zoo/homebrew-tap),
holding a single binary formula (`Formula/meerkat.rb`) that downloads
the tarballs this release publishes.

**Nothing in this repository's release workflow changes, and nothing
here needs to know the tap exists.** The tap pulls; meerkat does not
push. That is deliberate: goreleaser's `brews:`/`homebrew_casks:`
blocks would require a cross-repo write token to live in *this* repo's
secrets, which is exactly the kind of credential a release pipeline
should not be holding. The bump runs in the tap instead, with only its
own contents in scope.

The tap's `bump.yml` workflow:

1. runs on a 6-hourly cron (so a release is picked up within ~6 hours
   with no action from anyone) or on manual dispatch;
2. resolves the target tag — the newest `vX.Y.Z` release, or the one
   given to the dispatch;
3. downloads `meerkat_<v>_checksums.txt` and its
   `...sigstore.json` bundle and **cosign-verifies the bundle against
   the `release.yml` workflow identity** — the same check documented in
   [Verifying a release](#verifying-a-release-consumer-side) below —
   before reading a single hash out of that file;
4. pins the per-platform SHA-256s into the formula, then runs
   `brew style`, `brew audit --strict`, `brew install` and `brew test`
   against the regenerated formula on the runner, and only if all of
   that passes pushes the bump commit to the tap's `main`.

If the signature doesn't verify, the workflow fails and the formula
keeps pointing at the previous release: a bad or unsigned release can
never be pinned into the tap.

To publish the bump immediately rather than waiting for the cron:

```sh
gh workflow run bump.yml --repo zegit-zoo/homebrew-tap -f tag=vX.Y.Z
```

Users on a brew install upgrade with `brew upgrade meerkat`; `mk
update` refuses there (see
[docs/INSTALL.md](INSTALL.md#updating-a-homebrew-install)), so the tap
being stale is the only thing standing between a release and its
Homebrew users — worth the manual dispatch on a release you care about
landing fast.

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
