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
- [Install on Windows](#install-on-windows)
- [Verify the download](#verify-the-download)
- [From source](#from-source)
- [Updating](#updating)
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
signature. To verify before installing (macOS / Linux):

```bash
TAG=$(gh release list --repo zegit-zoo/meerkat -L 1 --json tagName -q '.[0].tagName')  # latest tag
VERSION=${TAG#v}
PLATFORM=darwin_arm64   # adjust for your platform

# 1. fetch the checksums, signature, and certificate via gh CLI
gh release download "$TAG" \
  --repo zegit-zoo/meerkat \
  -p "meerkat_${VERSION}_checksums.txt" \
  -p "meerkat_${VERSION}_checksums.txt.sig" \
  -p "meerkat_${VERSION}_checksums.txt.pem"

# 2. verify the cosign signature
cosign verify-blob \
  --certificate-identity-regexp '^https://github\.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  --signature "meerkat_${VERSION}_checksums.txt.sig" \
  --certificate "meerkat_${VERSION}_checksums.txt.pem" \
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

# 1. fetch checksums + signature + certificate
gh release download $TAG `
  --repo zegit-zoo/meerkat `
  -p "meerkat_${VERSION}_checksums.txt" `
  -p "meerkat_${VERSION}_checksums.txt.sig" `
  -p "meerkat_${VERSION}_checksums.txt.pem"

# 2. verify signature
cosign verify-blob `
  --certificate-identity-regexp '^https://github\.com/zegit-zoo/meerkat/\.github/workflows/release\.yml@refs/tags/v' `
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' `
  --signature "meerkat_${VERSION}_checksums.txt.sig" `
  --certificate "meerkat_${VERSION}_checksums.txt.pem" `
  "meerkat_${VERSION}_checksums.txt"

# 3. verify the zip hash matches its line in checksums.txt
$expected = (Get-Content "meerkat_${VERSION}_checksums.txt" | Where-Object {
  $_ -match "meerkat_${VERSION}_${PLATFORM}.zip"
}) -split '\s+' | Select-Object -First 1
$Zip = "meerkat_${VERSION}_${PLATFORM}.zip"
$actual = (Get-FileHash -Algorithm SHA256 $Zip).Hash.ToLower()
if ($expected -ne $actual) { throw "SHA256 mismatch for $Zip" } else { 'OK' }
```

`cosign verify-blob` exits 0 only when the signature is present in
the Rekor transparency log AND the OIDC certificate identity matches
the `release.yml` workflow in this repository. Skip step 2 if you
don't need supply-chain attestation (but we recommend keeping it).

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
| **Windows:** `mk update`: `permission denied writing to ...` | Binary installed under `%ProgramFiles%` or another machine-wide path | Re-run from an elevated PowerShell, or move install to `%LOCALAPPDATA%\Programs\meerkat` (see [Why `%LOCALAPPDATA%\Programs\meerkat`](#why-localappdataprograms-meerkat-and-not-programfiles)). |
| **Windows:** `Expand-Archive` fails with "ZIP archive is not in correct format" | Truncated download | Re-run the install; if it keeps happening you may be hitting the anonymous API rate limit — `gh auth login` in PowerShell for a higher one. |
| **Windows:** `mk update`: leftover `meerkat.exe.old` in the install dir | Previous update completed normally — the `.old` file is removed on the next `mk update` run | Run `mk update` again, or delete `meerkat.exe.old` manually if you don't plan to update again soon. |
