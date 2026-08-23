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

## Open questions / follow-ups

- **Exact version number (`v0.9.0` vs `v1.0.0`):** left as a release-time
  decision for whoever cuts the tag; this spec only requires it to exceed
  every version already deployed.
- **Fork-specific downgrade guards:** if a fork's own `mk update` fork has a
  stricter or differently-shaped guard than the one added here, that fork's
  maintainers still need to confirm their guard treats the new upstream tag
  as an upgrade before converging — this spec can only fix the guard that
  ships in this repository.
