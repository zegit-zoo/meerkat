# Spec: Migration path from downstream v0.8.x to upstream SemVer releases

**Status:** Proposed · **Scope:** release tagging + `mk update`'s token
resolution and downgrade guard · **Tracks:** issue #13

## Summary

This repository (`zegit-zoo/meerkat`) is currently tagged `v0.2.0`. Several
downstream forks have deployed the CLI well past that — release tags up to
`v0.8.6`, including keyring-based authentication fixes and downgrade
protection added independently in those forks. When a downstream fork
eventually wants its fleet to converge back onto upstream releases, those
clients will run `mk update` against *this* repository's release feed. If
upstream's next tag were numerically lower than what a client is already
running, any downgrade guard (here or in a fork) would correctly refuse the
"upgrade" — which is the right behavior for a real downgrade, but the wrong
outcome for what should be a clean migration.

This spec covers the two things needed to make that migration a non-event:

1. **Client side:** `mk update`'s version comparison and token resolution
   need to be correct and robust enough that they behave identically
   whether the "current" version came from this repo's own tag history or
   from a fork's independently-numbered series.
2. **Release side:** upstream's next tag needs to be numbered so that it is
   unambiguously an upgrade for every version already in the wild — not
   just for `v0.2.x` deployments.

## Background — state before this spec

- `internal/update/notify.go` compared versions with a hand-rolled
  dot-split-and-compare (`splitVersion`/`isNewer`) that only looked at
  `MAJOR.MINOR.PATCH` and silently discarded any pre-release suffix, even
  when comparing two pre-releases of the same core version.
- `internal/cli/update.go` had no downgrade guard at all — `--force` existed
  and was already documented as "downgrade-friendly," but nothing enforced
  the non-`--force` path staying at or above the current version. It only
  special-cased the exact-same-version re-install case.
- `internal/auth` resolved a GitHub token exactly one way: shell out to the
  cached `gh auth token`. No fallback existed for environments where `gh`
  isn't installed or authenticated — which matters more once we expect
  deployments that were never necessarily set up with `gh` in the first
  place (containers, service accounts, fork-specific provisioning).

None of this was wrong for a single, linearly-tagged repository. It becomes
a problem the moment a client's own version number can legitimately be
"ahead" of the repository it's about to pull an update from.

## Design

### 1. Pure SemVer comparison (`internal/update/semver.go`)

Replaced the old ad hoc compare with a real SemVer 2.0.0 precedence
implementation (`parseSemver`, `compareSemver`, exported `IsUpgrade` /
`IsDowngrade`). It is pure — a function of the three numeric components (and
pre-release identifiers, when present) with no repository-specific
special-casing, no upper bound, and no assumption about which repository
minted a given tag. Concretely:

- `IsUpgrade("v0.9.0", "v0.8.6")` → `true`. A client sitting on a downstream
  fork's `v0.8.6` correctly treats upstream's next `v0.9.0+` tag as an
  upgrade, purely because `9 > 8` in the minor component — it does not
  matter that `v0.8.6` never existed in this repository's own tag history.
- An unparseable version on either side (`"dev"`, `"unknown"`, `""`, a
  malformed tag) always returns `false` for both `IsUpgrade` and
  `IsDowngrade` — "can't tell" must never block an update, and must never be
  misreported as an upgrade either.
- Real pre-release precedence is now honored (`v0.4.0-rc1` is genuinely
  newer than `v0.4.0-rc0`) rather than ignored — see `docs/RELEASE.md`'s tag
  policy note: this repo's tags are always plain `vX.Y.Z` with no
  pre-release suffix today, so this mostly matters for forks or future
  policy changes, not the current release flow.

`notify.go`'s "a newer release is available" nag and `cli/update.go`'s
downgrade guard (below) both call into this same comparator, so there is
exactly one definition of "newer" in the codebase.

### 2. Downgrade guard (`internal/cli/update.go`)

`mk update` now refuses to install a target release that `update.IsDowngrade`
confirms is older than the running binary, unless `--force` is passed. This
formalizes what forks had already built independently, using the pure SemVer
comparator above rather than a bespoke check — so the guard that protects
users from an accidental downgrade is *the same logic* that lets a `v0.8.6`
client accept `v0.9.0` without needing `--force`. There is no special-case
carve-out for "the migration release"; it falls out naturally from SemVer
precedence being the only rule.

### 3. OS keyring fallback for update credentials (`internal/auth`)

`auth.NewDefault()` now tries the cached `gh` CLI token first (unchanged
behavior), and falls back to the OS-native credential store — macOS
Keychain, Linux Secret Service (D-Bus), Windows Credential Manager, via
[`github.com/zalando/go-keyring`](https://github.com/zalando/go-keyring) —
if `gh` isn't installed, isn't authenticated, or simply has nothing cached.

Resolution order for a GitHub token:

1. `gh auth token` (existing `GhProvider`, via the cached gh CLI OAuth
   token).
2. OS keyring lookup under service `mk-update`, account `<domain>` (default
   `github.com`) — new `KeyringProvider`.
3. Anonymous access. The release repository is public, so this always works;
   it's simply the lower unauthenticated GitHub API rate limit.

Every keyring failure mode — unsupported platform, no Secret Service/D-Bus
session, a locked or denied keychain, or simply no entry stored — degrades
to "no credential" and falls through to the next mechanism (ultimately,
anonymous access). It is never a hard failure: a broken or absent keyring
must not be able to break `mk update`. `KeyringProvider` is behind the same
`auth.TokenProvider` interface as `GhProvider`, and the OS keyring call
itself sits behind an injectable `keyringBackend` interface so tests never
touch a real keychain/Secret Service/Credential Manager.

This matters for the migration specifically because a downstream fork's
fleet may include hosts that were never set up with an interactive `gh
auth login` — service accounts, minimal containers, CI-driven update jobs —
where a token provisioned straight into the platform keyring is the more
natural fit than expecting a gh CLI session.

## Release-side steps for a clean migration

To make the next upstream tag land as an unambiguous upgrade for every
downstream client regardless of which fork or patch series it's on:

1. **Tag upstream at `v0.9.0` (preferred) or `v1.0.0`.** Either works
   mechanically — `IsUpgrade` only needs the new tag's MAJOR.MINOR.PATCH to
   numerically exceed the highest version any client might already be
   running. `v0.9.0` keeps room in the `0.x` line for further pre-1.0
   iteration; `v1.0.0` is the stronger signal that this is the point forks
   should converge on. Either choice must exceed `v0.8.6`, the highest
   version confirmed in the wild at the time of writing — do not tag
   anything in the `v0.8.x` range or lower.
2. **Follow the existing tag/release process unchanged** (`docs/RELEASE.md`):
   signed `vX.Y.Z` tag, the tag-protection ruleset (no pre-release suffixes,
   no moving tags), `verify` + `goreleaser` workflow, cosign-signed
   checksums. Nothing about the migration requires deviating from that
   pipeline — the whole point of the SemVer fix above is that the release
   process doesn't need special-casing.
3. **Communicate the version jump to fork maintainers/operators** so nobody
   is surprised that `mk update` (or `mk version`) suddenly reports a jump
   from `v0.2.0`-shaped history to `v0.9.0`/`v1.0.0` — this is a repository
   convergence event, not a claim that eight-tenths of a release's worth of
   upstream work shipped overnight.
4. **No downstream company names in this repository.** Any documentation,
   commit messages, or release notes describing this migration refer to
   "downstream forks" generically — never a specific deployment or
   organization name.

## Converging from a downstream fork

**Status:** Implemented · **Scope:** `cmd/meerkat-bootstrap`,
`internal/update` (shared primitives), `.goreleaser.yaml` · **Tracks:**
issue #29

Everything above this section (issue #13) makes upstream SemVer safe to
converge onto *once a client is already running this repository's
updater*. It deliberately does not, and structurally cannot, solve the
other half of the problem this spec's own Summary called out: a binary
built by a downstream fork has an updater that is compiled to query that
fork's own release feed and trust that fork's own signing identity. No
tag number changes that — `IsUpgrade`/`IsDowngrade` never run at all if
the client's updater never asks `zegit-zoo/meerkat` in the first place.
Crossing that gap needs one out-of-band, verified step, run once per
host.

### Why a separate binary rather than extending `mk update`

The alternative — teaching `mk update` to accept an alternate
repository/feed argument — was explicitly rejected (see Non-goals in the
tracking issue): it would mean upstream's steady-state updater carries
permanent code paths for trusting an arbitrary repository and signing
identity, which is exactly the kind of trust surface this project's
threat model (`docs/RELEASE.md`, `internal/update/cosign.go`) tries to
keep as small and as pinned as possible. A separate, small, single-purpose
binary — used once, then never needed again on that host — keeps that
trust surface at zero in the steady state.

`meerkat-bootstrap` (`cmd/meerkat-bootstrap`) is that binary. It
deliberately does not reimplement any of the security-sensitive pieces
`mk update` already has, tested, in `internal/update`: OS/architecture
asset selection (`PickAssetName`/`PickAssetNameFor`), the HTTPS-only
redirect allowlist (`client.go`), checksum parsing, Sigstore bundle
verification pinned to this exact identity —

- **OIDC issuer:** `https://token.actions.githubusercontent.com`
- **Certificate identity:** `CertIdentityRegexp` in
  `internal/update/cosign.go`, i.e. `zegit-zoo/meerkat`'s own
  `release.yml` GitHub Actions workflow, running against an actual
  `refs/tags/vX.Y.Z` ref

— and the atomic write-to-temp-plus-rename install with backup/rollback
(`install.go`). `meerkat-bootstrap` calls those functions directly; it
does not fork, vendor, or reimplement any part of them, so a fix to any
of that logic (a new redirect host, a stricter identity regexp, a
symlink-handling hardening) benefits `mk update` and `meerkat-bootstrap`
identically, from one change.

What's new in `internal/update` for this is scoped to primitives
`mk update` itself never needed, because it always operates on its own
`os.Executable()`, which always exists and is always the platform it's
currently running on:

- `PickAssetNameFor(version, goos, goarch)` / `ArchiveExt(goos)` — asset
  naming for a platform other than the one currently running, and the
  Windows `.zip` vs. everything-else `.tar.gz` distinction
  `.goreleaser.yaml`'s `archives.format_overrides` encodes. `PickAssetName`
  itself (`mk update`'s own entry point) is untouched.
- `ExtractMeerkatArchive` / `extractMeerkatZip` — a zip-extraction path
  alongside the existing tar.gz one (`ExtractMeerkat`, untouched), needed
  because `meerkat-bootstrap` must be able to install on Windows, which
  `mk update` today only reaches via `ExtractMeerkat`'s existing tar.gz
  path regardless of platform.
- `InstallAtomic` / `resolveDestination` / `RemoveBackup` / `RestoreBackup`
  — `swapWithBackup` (the core of `installStaged`, `mk update`'s own
  install step) factored out and reused, but operating on an arbitrary,
  resolved `--destination` instead of the running executable, and
  deliberately *not* deleting the `.old` backup immediately the way
  `installStaged` does on non-Windows — `meerkat-bootstrap` needs that
  backup to survive until its own post-install check passes.
- `RunVersionSmoke` / `DetectInstalledVersion` — run `<binary> version`
  (optionally `--json`) as a subprocess. `mk update` never needed this: it
  already knows its own version at compile time. `meerkat-bootstrap` does
  not know what's at an arbitrary `--destination` ahead of time, so it
  asks the binary itself, and falls back to parsing the first
  SemVer-shaped token out of plain `version` output for a fork binary
  that predates (or never added) a `--json` flag.

None of this changes `PickAssetName`, `ExtractMeerkat`, `installStaged`,
or `SwapAndReExec`'s own observable behavior — `mk update`'s existing
tests are unmodified and green.

### Downgrade guard and rollback

`meerkat-bootstrap`'s downgrade guard is the same `IsDowngrade` this spec
already established as the single source of truth for "newer" — a
`--release` older than the version `DetectInstalledVersion` reports at
`--destination` is refused unless `--force` is passed, and (as this
spec's whole point) a downstream `v0.8.x` build accepts an upstream
`v0.9.0+` target with no `--force` needed.

Unlike `mk update` (which re-execs into the new binary immediately after
a successful swap, on the premise that a binary which already passed
cosign + SHA-256 verification is trustworthy enough to hand control to),
`meerkat-bootstrap` keeps `<destination>.old` until it has independently
confirmed the newly installed binary actually runs
(`RunVersionSmoke`/`<destination> version`), and automatically restores
that backup — via `RestoreBackup` — if that check, the swap itself, or
verification upstream of the swap ever fails. See
[`docs/INSTALL.md`'s "Converging from a downstream
fork"](../INSTALL.md#converging-from-a-downstream-fork) for the exact
operator-facing command, rollback behavior, and how to confirm `mk update
--check` now queries `zegit-zoo/meerkat`.

### Release publication

`.goreleaser.yaml` builds `meerkat-bootstrap` as a second `builds:` entry
from the same commit, same workflow run, and therefore the same cosign
identity as `meerkat` itself — it is not a separately signed or
separately trusted artifact. It publishes as a standalone binary
(`archives: formats: [binary]`) rather than wrapped in a tar.gz/zip,
named `meerkat-bootstrap_<version>_<os>_<arch>[.exe]`, and participates in
the same `checksum:` file and per-artifact SBOM generation as the
`meerkat` archives. The existing `meerkat` build, its archives, and the
release workflow's `docker` job are untouched.

## Testing

- `internal/update/semver_test.go`: `TestIsUpgrade` and `TestIsDowngrade`
  cover the full precedence matrix, including the migration case directly
  (`IsUpgrade("v0.9.0", "v0.8.6") == true`, `IsUpgrade("v1.0.0", "v0.8.6") ==
  true`), numeric-vs-lexical pitfalls (`v0.8.10` > `v0.8.6`, `v0.10.0` >
  `v0.9.9`), and pre-release precedence.
- `internal/auth/keyring_test.go` and `default_test.go`: fake `keyringBackend`
  and stub `TokenProvider`s cover the fallback ordering (gh preferred,
  keyring used when gh errors or is empty) and confirm every keyring error
  path degrades to a non-fatal `ErrNoConfig`/`ErrNoToken` rather than
  propagating a distinguishable "fatal" error.
- `internal/update/bootstrap_test.go`: `InstallAtomic` replacing a real
  file and a symlinked destination (proving the real target is replaced
  and neighbouring files/the symlink entry itself are untouched),
  `RestoreBackup` round-tripping after a failed `RunVersionSmoke`,
  `DetectInstalledVersion`'s `--json` and plain-text-fallback paths
  against fixture shell-script binaries (including one shaped like the
  acceptance criteria's "downstream `v0.8.6`, no GitHub updater support"
  fixture), and two composition tests
  (`TestBootstrapFlow_TamperedChecksumRejectedBeforeInstall`,
  `TestBootstrapFlow_UnsignedBundleRejectedBeforeChecksumIsTrusted`) that
  run the fetch → download → verify sequence against a fake release
  server to confirm a tampered checksum or signature is rejected before
  any install step runs.
- `internal/update/release_test.go` / `download_test.go`:
  `PickAssetNameFor`/`ArchiveExt` across all six published
  darwin/linux/windows × amd64/arm64 combinations, and
  `ExtractMeerkatArchive`'s zip-vs-tar.gz dispatch (including a
  zip-specific oversize-entry rejection test).
- `cmd/meerkat-bootstrap/install_test.go`: the CLI-level downgrade
  refusal / `--force` decision (`decideProceed`) and the `--destination`
  default-to-`$PATH` resolution (`resolveDestinationFlag`), both pure and
  tested without any network dependency — mirroring this codebase's
  existing convention of keeping the thin CLI layer's own tests network-
  free and pushing verification-heavy testing into `internal/update`.

## Open questions / follow-ups

- **Exact version number (`v0.9.0` vs `v1.0.0`):** left as a release-time
  decision for whoever cuts the tag; this spec only requires it to exceed
  every version already deployed.
- **Fork-specific downgrade guards:** if a fork's own `mk update` fork has a
  stricter or differently-shaped guard than the one added here, that fork's
  maintainers still need to confirm their guard treats the new upstream tag
  as an upgrade before converging — this spec can only fix the guard that
  ships in this repository.
