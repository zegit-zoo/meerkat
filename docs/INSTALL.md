# Install

Meerkat ships as a single static binary for **darwin / linux / windows ×
amd64 / arm64**. Releases live on the project's
[Releases page](https://github.com/zegit-zoo/meerkat/releases).
Unix releases are `.tar.gz`; Windows releases are `.zip`.

The repo is public, so downloading release assets — and running
`mk update` — is anonymous; no GitHub login or token required. A token
(via `gh auth login` or a `GH_TOKEN` env var) is optional: it only
raises GitHub's API rate limit (60/hr anonymous vs. 5000/hr
authenticated). See [Updating](#updating).

Jump to:

- [Install on macOS / Linux](#install-on-macos--linux)
- [Homebrew `mk` collision](#homebrew-mk-collision)
- [Atomic installs (macOS code signatures)](#atomic-installs-macos-code-signatures)
- [Install on Windows](#install-on-windows)
- [Verify the download](#verify-the-download)
- [From source](#from-source)
- [Updating](#updating)
- [Converging from a downstream fork](#converging-from-a-downstream-fork)
- [Troubleshooting](#troubleshooting)

## Install on macOS / Linux

No login required — the repo is public. Install `gh` from
<https://cli.github.com> if you don't have it, or use the curl/wget
alternative below.

```bash
# Pick your platform
PLATFORM=darwin_arm64    # darwin_arm64 / darwin_amd64 / linux_amd64 / linux_arm64

# Install to a user-writable directory (recommended).
mkdir -p ~/.local/bin

# Download and extract the latest release (anonymous; no tag pinned —
# `gh release download` with no tag argument fetches the latest release).
gh release download \
  --repo zegit-zoo/meerkat \
  -p "meerkat_*_${PLATFORM}.tar.gz" \
  --output - \
  | tar -xz -C ~/.local/bin meerkat

ln -sf meerkat ~/.local/bin/mk      # convenience short alias

# Ensure ~/.local/bin is on $PATH (add to ~/.zshrc or ~/.bashrc):
case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) export PATH="$HOME/.local/bin:$PATH";; esac

meerkat version
```

### Without `gh` (plain curl/wget)

```bash
PLATFORM=darwin_arm64    # darwin_arm64 / darwin_amd64 / linux_amd64 / linux_arm64
mkdir -p ~/.local/bin

# Resolve the latest release tag from the public API (anonymous, rate-limited
# to 60 req/hr/IP — pass a token via `Authorization: Bearer <token>` for more).
TAG=$(curl -fsSL https://api.github.com/repos/zegit-zoo/meerkat/releases/latest \
  | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')

curl -fsSL "https://github.com/zegit-zoo/meerkat/releases/download/${TAG}/meerkat_${TAG#v}_${PLATFORM}.tar.gz" \
  | tar -xz -C ~/.local/bin meerkat

ln -sf meerkat ~/.local/bin/mk
meerkat version
```

### Why `~/.local/bin` and not `/usr/local/bin`?

`mk update` downloads and verifies the new release as the current
user, stages it in a private temp directory, then performs the final
copy/move into the install directory. If that final directory is
root-owned, `mk update` prompts through `sudo` for only those final
filesystem operations. The default for "system-wide" installs differs
by platform:

| Path | Owner (default) | `mk update` without sudo? |
|------|----------------|----------------------------|
| `~/.local/bin` | you | ✓ |
| `/opt/homebrew/bin` (Apple Silicon) | you | ✓ |
| `/usr/local/bin` (Intel macOS) | `root:wheel` | ✗ requires sudo |
| `/usr/local/bin` (Apple Silicon macOS) | `root:wheel` | ✗ requires sudo |
| `/usr/bin`, `/bin` (Linux) | `root:root` | ✗ requires sudo |

If you already installed to a root-owned directory and want to
move to `~/.local/bin` so future `mk update`s don't need sudo:

```bash
sudo mv /usr/local/bin/meerkat ~/.local/bin/meerkat
sudo rm -f /usr/local/bin/mk
ln -sf meerkat ~/.local/bin/mk
```

`mk update` no longer needs to be re-run with `sudo`: it keeps the
download and signature verification unprivileged, then uses `sudo
cp`/`sudo mv` only if the install directory requires it.

### Homebrew `mk` collision

`brew` ships a formula called `mk` — the unrelated Plan 9 `mk` build
tool (`homebrew/core/mk`). If it's installed, it places its own `mk`
executable in Homebrew's bin directory (`/opt/homebrew/bin` on Apple
Silicon, `/usr/local/bin` on Intel). Meerkat's `mk` is just a
convenience symlink to `meerkat`, so if both end up on `$PATH`,
whichever directory comes first wins — silently, and without an
error. Which `mk` runs depends entirely on shell startup order, not
on which one you meant.

Check what actually resolves:

```bash
which -a mk        # lists every `mk` on $PATH, in resolution order
type -a mk         # also reports shell aliases/functions named `mk`
```

To avoid the collision:

- **Prefer explicit `$PATH` ordering.** Keep `~/.local/bin` ahead of
  Homebrew's bin directory in your shell rc (`~/.zshrc` / `~/.bashrc`)
  so meerkat's `mk` always wins:

  ```bash
  export PATH="$HOME/.local/bin:/opt/homebrew/bin:$PATH"
  ```

- **Or don't rely on the shorthand at all.** Skip the `mk` symlink
  and always invoke `meerkat` directly in scripts and mkfiles that
  also need Plan 9 `mk` on the same machine.
- **Or alias it per-shell instead of relying on `$PATH`.** A shell
  alias resolves before `$PATH` lookup, so it wins regardless of
  directory order:

  ```bash
  alias mk=meerkat   # add to ~/.zshrc / ~/.bashrc
  ```

Never install meerkat's `mk` symlink into a directory Homebrew
manages (e.g. `/opt/homebrew/bin`) by overwriting whatever is already
there in place — see [Atomic installs](#atomic-installs-macos-code-signatures)
below for why.

### Atomic installs (macOS code signatures)

`make install` replaces the destination binary and the `mk` symlink
via a temp-file-then-rename: the new file is written next to the
target under a throwaway name in the *same* directory, then moved
into place with `mv -f` (a `rename(2)`, atomic on the same
filesystem) rather than being copied on top of whatever is already
there. This matters for two reasons:

1. **Code-signature invalidation.** Overwriting an existing file
   in place (e.g. `cp new-binary /path/to/existing-binary`) truncates
   and rewrites the destination's bytes through its existing inode.
   On macOS this can invalidate the ad-hoc/Developer ID code
   signature that's checked against that exact file, and can trip
   Gatekeeper or EDR tooling on next launch. A rename instead swaps
   in a whole new, already-signed file atomically.
2. **Symlink safety.** If the destination path is already a symlink
   (for example, one pointing at a Homebrew-managed `mk`), writing
   into it in place follows the link and clobbers whatever it points
   to — not what you intended to install. A `rename(2)` onto that
   path replaces the symlink entry itself; it never follows it.

If you're scripting your own install/update outside of `make
install`, use the same pattern: write to a temp file in the
destination directory, then `mv -f` it over the final path.

## Install on Windows

No login required — the repo is public (install `gh` from
<https://cli.github.com> if you don't have it; PowerShell or Git Bash
both work).

Run the install from **PowerShell**:

```powershell
# Pick your platform
$PLATFORM = 'windows_amd64'             # windows_amd64 / windows_arm64

# Install to a user-writable directory (recommended).
$Dest = "$env:LOCALAPPDATA\Programs\meerkat"
New-Item -ItemType Directory -Force -Path $Dest | Out-Null

# Download the latest release zip (anonymous; no tag pinned).
$Zip = "$env:TEMP\meerkat_${PLATFORM}.zip"
gh release download `
  --repo zegit-zoo/meerkat `
  -p "meerkat_*_${PLATFORM}.zip" `
  --output $Zip

Expand-Archive -LiteralPath $Zip -DestinationPath $Dest -Force
Remove-Item $Zip

# Create the `mk.exe` convenience alias.
Copy-Item -Force "$Dest\meerkat.exe" "$Dest\mk.exe"

# Add $Dest to your user PATH (persistent across new shells).
$current = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($current -notlike "*$Dest*") {
  [Environment]::SetEnvironmentVariable('Path', "$current;$Dest", 'User')
  Write-Host "Added $Dest to user PATH. Open a new PowerShell for it to take effect."
}

& "$Dest\meerkat.exe" version
```

### Why `%LOCALAPPDATA%\Programs\meerkat` and not `%ProgramFiles%`?

Same reasoning as `~/.local/bin` on Unix: `mk update` performs an
in-place swap of the running binary, which requires write access
to the install directory. `%ProgramFiles%` is write-protected unless
you're elevated; `%LOCALAPPDATA%\Programs\meerkat` is user-owned
and `mk update` works without UAC.

| Path | Owner (default) | `mk update` without elevation? |
|------|----------------|--------------------------------|
| `%LOCALAPPDATA%\Programs\meerkat` | you | ✓ |
| `%USERPROFILE%\bin` | you | ✓ |
| `%ProgramFiles%\meerkat` | `TrustedInstaller` | ✗ requires elevated PowerShell |
| `C:\Windows\System32` | `TrustedInstaller` | ✗ requires elevated PowerShell |

If you previously installed to `%ProgramFiles%` and want to move
to `%LOCALAPPDATA%\Programs\meerkat` so future updates work without
elevation, run an **elevated** PowerShell once:

```powershell
# PowerShell run as Administrator
$Src  = "$env:ProgramFiles\meerkat"
$Dest = "$env:LOCALAPPDATA\Programs\meerkat"
New-Item -ItemType Directory -Force -Path $Dest | Out-Null
Move-Item -Force "$Src\*.exe" $Dest
Remove-Item -Recurse -Force $Src
```

Then update PATH (in a non-elevated shell):

```powershell
$current = [Environment]::GetEnvironmentVariable('Path', 'User')
$current = ($current -split ';' | Where-Object { $_ -notlike "*\meerkat" }) -join ';'
[Environment]::SetEnvironmentVariable('Path', "$current;$env:LOCALAPPDATA\Programs\meerkat", 'User')
```

### Windows self-update mechanics

Windows can't replace a *running* executable in-place the way Unix
can. `mk update` handles this transparently: it renames the running
`meerkat.exe` to `meerkat.exe.old`, drops the new binary in its
place, then launches the new binary as a child process and waits for
it. You'll see the new binary's output stream back to your terminal
inline — no extra step required, and the `meerkat.exe.old` file is
cleaned up the next time you run `mk update`.

If for any reason the update aborts after the rename but before the
relaunch (very rare; usually only if your terminal kills the process
mid-flight), you can recover by manually renaming
`meerkat.exe.old` back to `meerkat.exe` in the install directory.

## Verify the download

Each release publishes a SHA256 checksums file plus a cosign keyless
signature bundle. To verify before installing (macOS / Linux):

```bash
TAG=$(gh release list --repo zegit-zoo/meerkat -L 1 --json tagName -q '.[0].tagName')  # latest tag
VERSION=${TAG#v}
PLATFORM=darwin_arm64   # adjust for your platform

# 1. fetch the checksums file and its cosign Sigstore bundle via gh CLI
gh release download "$TAG" \
  --repo zegit-zoo/meerkat \
  -p "meerkat_${VERSION}_checksums.txt" \
  -p "meerkat_${VERSION}_checksums.txt.sigstore.json"

# 2. verify the cosign signature
cosign verify-blob \
  --certificate-identity-regexp '^https://github\.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  --bundle "meerkat_${VERSION}_checksums.txt.sigstore.json" \
  "meerkat_${VERSION}_checksums.txt"

# 3. verify the tarball matches its line in checksums.txt
sha256sum --check --ignore-missing "meerkat_${VERSION}_checksums.txt"
```

On **Windows** the same flow works in PowerShell with `cosign.exe`
installed (e.g. via `winget install sigstore.cosign`):

```powershell
$TAG      = (gh release list --repo zegit-zoo/meerkat -L 1 --json tagName -q '.[0].tagName')  # latest tag
$VERSION  = $TAG.TrimStart('v')
$PLATFORM = 'windows_amd64'

# 1. fetch checksums file + cosign Sigstore bundle
gh release download $TAG `
  --repo zegit-zoo/meerkat `
  -p "meerkat_${VERSION}_checksums.txt" `
  -p "meerkat_${VERSION}_checksums.txt.sigstore.json"

# 2. verify signature
cosign verify-blob `
  --certificate-identity-regexp '^https://github\.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$' `
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' `
  --bundle "meerkat_${VERSION}_checksums.txt.sigstore.json" `
  "meerkat_${VERSION}_checksums.txt"

# 3. verify the zip hash matches its line in checksums.txt
$expected = (Get-Content "meerkat_${VERSION}_checksums.txt" | Where-Object {
  $_ -match "meerkat_${VERSION}_${PLATFORM}.zip"
}) -split '\s+' | Select-Object -First 1
$Zip = "meerkat_${VERSION}_${PLATFORM}.zip"
$actual = (Get-FileHash -Algorithm SHA256 $Zip).Hash.ToLower()
if ($expected -ne $actual) { throw "SHA256 mismatch for $Zip" } else { 'OK' }
```

`cosign verify-blob` exits 0 only when the bundle's embedded Rekor
transparency-log inclusion proof is valid AND the OIDC certificate
identity matches the `release.yml` workflow in this repository. The
transparency-log check is verified offline against the proof
embedded in the bundle — no live call to Rekor is required at verify
time. Skip step 2 if you don't need supply-chain attestation (but we
recommend keeping it).

## From source

```bash
git clone https://github.com/zegit-zoo/meerkat.git
cd meerkat
make build              # → bin/meerkat (and bin/mk symlink)
make install            # → ~/.local/bin/{meerkat,mk}
make test               # run the test suite
make security           # govulncheck + gosec + gitleaks
make smoke              # end-to-end CLI sanity check
make docs               # regenerate docs/CLI.md
```

## Updating

```bash
mk update --check                      # latest version + current
mk update                              # download + verified swap + re-exec
mk update --version <TAG>              # pin to a specific tag, e.g. v1.2.3
mk update --force                      # downgrade or re-install
mk update --yes                        # skip confirmation
```

`mk update` works with no authentication — the repo is public. If
you've run `gh auth login`, `mk update` reuses the cached GitHub OAuth
token via the `gh` CLI for a higher API rate limit; `gh auth status`
shows the current state. The token is refreshed by gh automatically
— there are no PATs to manage either way.

On **Windows**, `mk update` cannot replace a running binary in place
the way Unix can. It uses a rename-then-relaunch pattern: the old
binary is renamed to `meerkat.exe.old`, the new binary takes the
original path, and the new binary is launched as a child process
that streams its output back to your terminal. To you it looks like
a normal update; the `.old` file is removed on the next `mk update`.

## Converging from a downstream fork

If your `meerkat`/`mk` binary was built by a downstream fork — a team or
project that maintains its own patch series on top of this codebase — `mk
update` on that binary cannot pull releases from this repository on its
own, no matter how new they are. Its updater was *compiled* to query a
different release feed and trust a different signing identity; that is a
build-time decision baked into the binary, not something a version number
can override. You need exactly one bootstrap step to cross that gap.
After that, the binary you're left with is a normal upstream build, and
every subsequent `mk update` works exactly as described above.

### Why this needs a separate tool, not just a newer tag

This repository's release tags follow plain SemVer precedence (see
[`docs/design/upstream-migration.md`](design/upstream-migration.md)), and
a tag here numbered `v0.9.0` or higher is guaranteed to compare as newer
than any known downstream fork release, including `v0.8.x` series builds.
That guarantee matters for exactly one thing: once a binary is already
running this repository's updater, it will never mistake a legitimate
upstream release for a downgrade. It does **not** — and structurally
cannot — help a binary whose updater doesn't query this repository at
all. SemVer ordering answers "is this a newer version of the release feed
I already trust"; it has nothing to say about a binary that trusts a
*different* feed and identity in the first place. Getting from there to
here is a one-time, out-of-band step — `meerkat-bootstrap`.

### `meerkat-bootstrap`

`meerkat-bootstrap` is a small, standalone binary published alongside
every release (see the release assets, or build it yourself with `go
build ./cmd/meerkat-bootstrap`). It does not reimplement any of `mk
update`'s security-sensitive logic — it calls the exact same,
already-tested `internal/update` code for OS/architecture asset
selection, the redirect allowlist, checksum and Sigstore verification,
and the atomic backup/rollback install. The only things it does
differently, because it is a bootstrap tool rather than a running binary
updating itself, are: it can target an arbitrary `--destination` path
instead of its own `os.Executable()`, and it runs a `version` smoke check
against the newly installed binary before it lets go of the backup.

```bash
# Typical case: replace whatever "meerkat" already resolves to on $PATH
# with the newest stable upstream release.
meerkat-bootstrap install --destination "$(command -v meerkat)"

# Pin a specific release instead of "newest stable".
meerkat-bootstrap install --release v0.10.0 --destination /usr/local/bin/meerkat

# The destination is already on a numerically higher downstream series
# (e.g. v0.8.6) than the upstream tag you're targeting for some other
# reason, or you're intentionally re-installing/downgrading:
meerkat-bootstrap install --destination /usr/local/bin/meerkat --force
```

If `--destination` is omitted, it defaults to the first of `meerkat`/`mk`
found on `$PATH` — the same binary the example above resolves explicitly.
If `--release` is omitted, it installs the newest stable release (the
same release GitHub's "latest" marker points at — no pre-releases or
drafts). Like `mk update`, this works anonymously against the public
repository; a cached `gh auth login` token is used automatically for the
higher API rate limit if present, but there is no flag to pass a token
directly — `meerkat-bootstrap` never accepts a static credential as a
command-line argument.

**What gets verified, and in what order, before anything is installed:**

1. The release asset matching your OS/architecture, the checksums file,
   and its Sigstore signature bundle are located strictly among
   `zegit-zoo/meerkat`'s own published release assets — never any other
   repository, and never a redirect to an untrusted host (the same
   allowlist `mk update` itself enforces: `github.com`, `*.github.com`,
   `*.githubusercontent.com`, over HTTPS only).
2. The checksums file's Sigstore bundle is verified with `cosign
   verify-blob`, keyless (Fulcio + Rekor), pinned to this exact identity:
   - **OIDC issuer:** `https://token.actions.githubusercontent.com`
   - **Certificate identity:**
     `^https://github\.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$`

     In words: the signature must have been minted by *this repository's*
     `release.yml` GitHub Actions workflow, running against an actual
     `vX.Y.Z` release tag — not a fork's workflow, not a differently
     named workflow in this same repository, not a branch push. This is
     the one piece of trust this whole process is bootstrapping *into* —
     everything after this step inherits it.
   - This step is **not skippable**. Unlike `mk update --skip-cosign`,
     `meerkat-bootstrap` has no SHA-256-only fallback, because this is
     the step that establishes trust in the upstream identity in the
     first place; there is nothing to fall back to that would still mean
     anything.
3. Only once that signature is verified does `meerkat-bootstrap` trust
   the checksums file's contents, and verify the downloaded asset's own
   SHA-256 against it.
4. Only then is anything written to `--destination`.

**Downgrade guard:** exactly like `mk update`, a `--release` numerically
older than what's detected at `--destination` (via `<destination>
version`) is refused unless you pass `--force`. Migrating a `v0.8.x`
downstream build onto a `v0.9.0+` upstream release is, by SemVer
precedence, an upgrade — it needs no `--force`.

**Install safety:** `--destination` may be a real file or a symlink (for
example, `mk` symlinked to `meerkat` in the same directory, the layout
[Install on macOS / Linux](#install-on-macos--linux) sets up by default).
`meerkat-bootstrap` resolves the symlink to its real target first and
replaces that — the same rename-based, never-write-through-a-symlink
approach `make install` and `mk update` both already use (see [Atomic
installs](#atomic-installs-macos-code-signatures) above) — so the
symlink itself, and anything else nearby, is left untouched.

### Rollback

The previous binary is kept as `<destination>.old` until the newly
installed one proves it actually runs (`<destination> version` exits
successfully). If verification fails, the install/swap fails, or that
final smoke check fails for any reason, `meerkat-bootstrap` automatically
restores `<destination>.old` back over `<destination>` and exits non-zero
— your previous binary is back exactly where it was, and nothing is left
half-installed. You do not need to intervene.

If `meerkat-bootstrap` itself is killed or crashes (SIGKILL, power loss)
at the exact moment between renaming the old binary out of the way and
promoting the new one — a very small window — you can recover manually
the same way the [Windows self-update
mechanics](#windows-self-update-mechanics) section above describes:
rename `<destination>.old` back to `<destination>`.

### Confirming the migration worked

```bash
mk update --check
```

should now report a `target:` line naming a `zegit-zoo/meerkat` release
tag, and its `current:` line should match the version
`meerkat-bootstrap` just installed. From this point on, `mk update` talks
to `zegit-zoo/meerkat` directly — `meerkat-bootstrap` has done its job and
you shouldn't need it again unless you're migrating another host.

See
[`docs/design/upstream-migration.md`](design/upstream-migration.md#converging-from-a-downstream-fork)
for the full design rationale behind this split (why the SemVer fix
alone isn't sufficient, and why this is a separate binary rather than a
flag on `mk update` itself).

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| `mk update`: `install directory requires elevated privileges` | Binary installed in a root-owned directory (typically `/usr/local/bin` or `/usr/bin`) | Enter your password when `sudo` prompts. Permanent fix: move install to `~/.local/bin` (see [Why `~/.local/bin`](#why-localbin-and-not-usrlocalbin) above). |
| `mk update`: `install with sudo failed` | `sudo` was unavailable, cancelled, or the final copy/move failed | Re-run `mk update` and complete the `sudo` prompt, or move install to `~/.local/bin`. |
| `mk update`: `GitHub returned 401/403 ... likely the anonymous API rate limit` | Too many unauthenticated requests from your IP (60/hr) | `gh auth login` once for the higher authenticated rate limit (5000/hr), or wait an hour |
| `mk update`: `GitHub returned 401 ... run gh auth login to refresh your token` | A cached `gh` token was sent and GitHub rejected it (expired/revoked) | Run `gh auth status` and re-authenticate; or `gh auth logout` to fall back to anonymous |
| `make build` / `make sync`: `submodule update <path>: ...` | `content-source.yaml` has `type: submodule` but the submodule isn't checked out | `make kb-init` (or `git submodule update --init <path>`) |
| Binary returns `meerkat (dev)` despite a tagged release | Built outside CI, no `git describe` tag | `git fetch --tags` then `make build` |
| Running `meerkat` from `~/.local/bin/` exits with code 137 (SIGKILL) and no output | Endpoint Detection & Response (Crowdstrike Falcon, SentinelOne, Defender for Endpoint, etc.) on a managed Mac silently kills exec from non-allowlisted paths | Move the binary to a path the EDR allowlists — typically `/opt/homebrew/bin/` or `/usr/local/bin/`. The binary content + signature is identical; only the path matters to the EDR policy. On a managed fleet, ask whoever owns the EDR policy to allowlist the install path, or to add a rule keyed on the release's cosign signature. |
| `meerkat` prints "a newer release is available" after every command and you don't want it | The post-run update-check (24h cached) is on by default for tagged builds | Set `MEERKAT_NO_UPDATE_CHECK=1` in your shell rc to silence it permanently. |
| **macOS:** `mk` runs the wrong tool (Plan 9 `mk` build tool instead of meerkat, or vice versa) | Homebrew's `mk` formula and meerkat's `mk` shorthand are both on `$PATH`; the first directory in `$PATH` wins silently | Run `which -a mk` to see the resolution order, then fix `$PATH` ordering or use `alias mk=meerkat` — see [Homebrew `mk` collision](#homebrew-mk-collision). |
| **Windows:** `mk update`: `permission denied writing to ...` | Binary installed under `%ProgramFiles%` or another machine-wide path | Re-run from an elevated PowerShell, or move install to `%LOCALAPPDATA%\Programs\meerkat` (see [Why `%LOCALAPPDATA%\Programs\meerkat`](#why-localappdataprograms-meerkat-and-not-programfiles)). |
| **Windows:** `Expand-Archive` fails with "ZIP archive is not in correct format" | Truncated download | Re-run the install; if it keeps happening you may be hitting the anonymous API rate limit — `gh auth login` in PowerShell for a higher one. |
| **Windows:** `mk update`: leftover `meerkat.exe.old` in the install dir | Previous update completed normally — the `.old` file is removed on the next `mk update` run | Run `mk update` again, or delete `meerkat.exe.old` manually if you don't plan to update again soon. |
