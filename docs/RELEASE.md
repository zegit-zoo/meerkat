# Release process

## CI pipeline

Every push to `master` and every pull request runs the repo's gates as
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
2. Tag the release commit with `scripts/release-tag.sh`:

   ```sh
   bash scripts/release-tag.sh v1.2.3            # origin/master's head
   bash scripts/release-tag.sh v1.2.3 <commit>   # an earlier master commit
   ```

   The script fetches `master` and the tags, then refuses before
   tagging when:

   - the name is not `vX.Y.Z`;
   - the commit is not on `master` (the workflow refuses any other);
   - the tag already exists;
   - the commit's checks failed or are still pending (`--force-ci`
     overrides).

   It warns when the version is not the highest (an older-line patch).
   It prints the tag, the commit and the commits since the previous
   tag, and asks before acting (`--yes` skips the prompt). Then it
   creates a signed tag (`git tag -s`), verifies the signature, and
   pushes it over HTTPS, because an SSH key can map to a GitHub account
   the tag ruleset refuses. The push runs with `umask 022` (meerkat#203)
   and through the pre-push hooks. If the push fails, it deletes the
   local tag so a retry starts clean.

   Only Markus and Jonas can create `v*` tags (see
   [Tag protection](#tag-protection)).

3. The `release.yml` workflow triggers on the tag push. `verify` runs
   first; `goreleaser` and `docker` then each wait for approval in the
   `release` environment. The approval must come from the other release
   manager: the environment prevents self-review, so whoever pushed the
   tag cannot approve its release. Once approved, the two run in
   parallel. The jobs:
   - **`verify` job** — first fails unless the tagged commit is an ancestor of `origin/master`, then re-runs the full gate (lint + test + vuln + gosec + gitleaks) on the exact tagged SHA.
   - **`goreleaser` job** (runs only after `verify` passes) — builds cross-platform binaries (darwin/linux/windows, amd64/arm64), creates archives and a checksums file, generates SPDX SBOMs via syft, signs the checksums file with cosign (keyless, GitHub OIDC), and publishes a GitHub Release with all artifacts attached.
   - **`chart` job** (runs only after both `goreleaser` and `docker` succeed, so the image it defaults to exists) — packages `charts/meerkat` as version `X.Y.Z` with `appVersion` `X.Y.Z`, pushes it to `oci://ghcr.io/zegit-zoo/charts/meerkat`, signs its digest with cosign (keyless, GitHub OIDC) and attaches a SLSA provenance attestation. It has no `environment:` of its own, because a job that starts after the approval would ask for a second one; depending on both approved jobs keeps it behind the same approval.
   - **`docker` job** (runs only after `verify` passes, in parallel with `goreleaser`) — builds the hardened multi-stage [`Dockerfile`](../Dockerfile) for `linux/amd64` + `linux/arm64` via QEMU + buildx, pushes the manifest list to `ghcr.io/zegit-zoo/meerkat`, signs it with cosign (keyless, GitHub OIDC), and attaches a Syft SBOM + SLSA provenance attestation via buildx's native `--sbom`/`--provenance`. See [docs/CONTAINER.md](CONTAINER.md).

The released artifacts are:

- `meerkat_<v>_<os>_<arch>.tar.gz` / `.zip` (Windows)
- `meerkat-bootstrap_<v>_<os>_<arch>` / `.exe` (Windows) — a standalone
  binary (not archived) that installs a verified `zegit-zoo/meerkat`
  release independently of whatever updater the binary at its
  `--destination` understands; see [docs/INSTALL.md's "Converging from a
  downstream fork"](INSTALL.md#converging-from-a-downstream-fork)
- `meerkat_<v>_source.tar.gz` (the tagged source tree, `git archive`)
- `meerkat_<v>_checksums.txt` (SHA-256, covering every archive,
  standalone binary and SBOM in the release)
- `meerkat_<v>_checksums.txt.sigstore.json` (cosign Sigstore bundle: signature + certificate + Rekor inclusion proof)
- `*.sbom.json` (SPDX SBOM per archive, per standalone binary, and
  `meerkat_<v>_source.sbom.json` for the source archive)
- a SLSA build-provenance attestation for every file in the checksums
  file, stored on GitHub, not as a release asset:
  `gh attestation verify <file> -R zegit-zoo/meerkat`
- `ghcr.io/zegit-zoo/meerkat:X.Y.Z` / `:X.Y` (multi-arch OCI image,
  signed + attested), plus `:latest` when the tag is the highest
  release tag. Note these image tags drop the git tag's leading `v`
  — see [docs/CONTAINER.md](CONTAINER.md)
- `oci://ghcr.io/zegit-zoo/charts/meerkat:X.Y.Z` (the Helm chart, signed +
  attested; version and `appVersion` are both `X.Y.Z`). In git the chart
  carries the placeholder `0.0.0-dev`: never bump it by hand

The binaries and archives are reproducible: the embedded build date
and every file timestamp are the tagged commit's, and GoReleaser is
pinned to an exact version in `release.yml`, so rebuilding a tag at
its commit with the same GoReleaser and Go versions yields the same
archive and binary checksums.

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

Users on a brew install upgrade with `brew update && brew upgrade meerkat`; `mk
update` refuses there (see
[docs/INSTALL.md](INSTALL.md#updating-a-homebrew-install)), so the tap
being stale is the only thing standing between a release and its
Homebrew users — worth the manual dispatch on a release you care about
landing fast.

## Tag protection

`v*` tags are governed by the repository ruleset `release-tags-immutable`:

- the tagged **commit** must carry a verified signature. The ruleset does
  not check the tag itself, so a lightweight tag passes, and so does any
  commit GitHub signed on creation (API commits and PR merges)
- name must match `vX.Y.Z` exactly
- no deletion, no moving a tag once pushed
- creation is restricted: only Markus and Jonas can create a `v*` tag
  (the ruleset's bypass list holds the two human release managers, no
  apps or bots)

A tag on an unmerged commit still cannot become a release: the `verify`
job's first step fails unless the tagged commit is on `master`, and
nothing is built or signed before `verify` passes. The publishing jobs
(`goreleaser`, `docker`) run in the `release` environment, which requires
approval from Markus or Jonas, excluding whoever pushed the tag, and only
deploys `vX.Y.Z` tags. Release tags are still signed by convention
(`git tag -s`), but no rule enforces it.

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

`mk update` performs this same check automatically and requires `cosign` on
`PATH`; its `--skip-cosign` escape hatch is gated behind
`MEERKAT_UPDATE_ALLOW_UNVERIFIED=1` and, on a terminal, a typed confirmation.
Verification inside the binary (no cosign prerequisite) is a follow-up.

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

## Verifying the Helm chart (consumer side)

```sh
cosign verify \
  --certificate-identity-regexp '^https://github.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/zegit-zoo/charts/meerkat:X.Y.Z
gh attestation verify oci://ghcr.io/zegit-zoo/charts/meerkat:X.Y.Z -R zegit-zoo/meerkat
```

The chart is signed by the same `release.yml` identity as the image. A Flux
`OCIRepository` can check it with `verify:`; see
[charts/meerkat/README.md](../charts/meerkat/README.md#flux).
