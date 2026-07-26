# Defender for Endpoint (macOS) — meerkat exclusions

Our Macs run Microsoft Defender for Endpoint. Unlike Windows, macOS
Defender has no WDAC/AppLocker-style application-control engine — it
uses **exclusions** (path / file / process) to stop the antivirus
engine from scanning or interfering with a given binary.

The common symptom that brings people here: `meerkat` exits with
**code 137 (SIGKILL)** and no output. That's the EDR killing exec
from a non-allowlisted path. Adding the install dir as an exclusion
fixes it. (The binary content is identical regardless of path — only
the path matters to the policy.)

These are **path-based** exclusions because meerkat is not
codesign-notarised with your organization's code-signing certificate. See
[`README.md`](./README.md) for the full rationale.

## Paths to exclude

The standard meerkat install locations on macOS:

| Path | Notes |
|---|---|
| `~/.local/bin/meerkat` and `~/.local/bin/mk` | Primary recommended install |
| `/opt/homebrew/bin/meerkat` and `/opt/homebrew/bin/mk` | Apple Silicon Homebrew prefix |
| `/usr/local/bin/meerkat` and `/usr/local/bin/mk` | Intel Homebrew prefix |

Because `~` expands per-user, the home-dir entries can't be expressed
as a single machine-wide literal. Two options:

1. **Directory exclusion** of `.local/bin` (relative) — Defender
   resolves it per user profile.
2. **Standardise on a non-home path** (`/opt/homebrew/bin` or
   `/usr/local/bin`) and exclude only that. This is also the
   EDR-friendly recommendation in `docs/INSTALL.md`.

## Option A — local `mdatp` commands (per machine / testing)

```bash
# File exclusions for the home-dir installs (run as the user):
mdatp exclusion folder add --path "$HOME/.local/bin"

# Machine-wide Homebrew prefixes:
sudo mdatp exclusion folder add --path /opt/homebrew/bin
sudo mdatp exclusion folder add --path /usr/local/bin

# Verify:
mdatp exclusion list
```

To remove:

```bash
mdatp exclusion folder remove --path "$HOME/.local/bin"
```

> Folder exclusions are broad (they exclude everything under the
> path, not just meerkat). `/opt/homebrew/bin` and `/usr/local/bin`
> already hold many user binaries, so a folder exclusion there is
> usually already in line with how the fleet is managed. If you need
> to scope tighter, use a **file** exclusion instead:
> `mdatp exclusion file add --path /opt/homebrew/bin/meerkat`
> (note: a file exclusion is by inode/path, so it must be re-added
> after `mk update` replaces the binary — folder exclusions don't
> have that problem, which is why they're preferred for a
> self-updating tool).

## Option B — MDM managed profile (Intune / Jamf, fleet-wide)

Defender for Endpoint on macOS reads exclusions from the
`com.microsoft.wdav` managed configuration. Push this via Intune
(Custom profile) or Jamf (Application & Custom Settings →
`com.microsoft.wdav`).

`managed.json` snippet (merge into your existing wdav config):

```json
{
  "antivirusEngine": {
    "exclusions": [
      {
        "$type": "folder",
        "path": "/opt/homebrew/bin"
      },
      {
        "$type": "folder",
        "path": "/usr/local/bin"
      },
      {
        "$type": "excludedPath",
        "isDirectory": true,
        "path": "/Users/*/.local/bin"
      }
    ]
  }
}
```

> The `/Users/*/.local/bin` wildcard form is honoured by recent
> Defender for Endpoint builds for per-user directories. If your
> Defender version doesn't expand the wildcard, fall back to
> standardising meerkat on `/opt/homebrew/bin` (Apple Silicon) or
> `/usr/local/bin` (Intel) fleet-wide and exclude only those.

## What this does NOT do

- It does **not** disable real-time protection globally — only the
  listed paths are excluded from scanning/interference.
- It does **not** vouch for the binary's provenance. Verify the
  download with cosign (see `docs/INSTALL.md` → "Verify the
  download") before trusting a given release; the exclusion only
  stops the EDR from killing an already-trusted binary.

## Future: codesign + notarise

If your organization obtains an Apple Developer ID and starts
codesigning + notarising the macOS binary in CI, these path
exclusions can be replaced with a signed-binary trust that survives
path changes and doesn't need a per-path carve-out. Until then,
path exclusions are the pragmatic choice — mirrors the Windows
path-rule decision in this folder.
