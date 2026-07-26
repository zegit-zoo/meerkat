# Microsoft Defender allowlisting for meerkat

This folder contains ready-to-import policy templates for Internal-IT
to allowlist the `meerkat` / `mk` binary in Microsoft Defender across
the platforms your organization runs (Windows **and** macOS — if your
Macs run Defender for Endpoint too).

All templates here are **path-based**. Read the next section to
understand why, and what the trade-off is.

## Read this first: what we sign, and what Defender can use

meerkat releases **are** signed — but with a signature Defender
**cannot** consume. The distinction matters:

| | What meerkat does | What Defender keys off |
|---|---|---|
| Signature type | **cosign keyless** (Sigstore) over `meerkat_<v>_checksums.txt` | **Authenticode** signature embedded in the `.exe` |
| Proves | The release came from the project's CI pipeline and wasn't tampered with (supply-chain provenance) | The binary was signed by a publisher trusted in the Windows certificate chain |
| Verified with | `cosign verify-blob` | The OS loader / Defender / SmartScreen / WDAC |
| Trust anchor | Fulcio cert + Rekor transparency log | A code-signing certificate (OV/EV) |

meerkat's binary is **not Authenticode-signed** — there is no
code-signing certificate in the build, by deliberate decision. So
Defender sees `meerkat.exe` as an unsigned binary from an unknown
publisher, and a **publisher-based** allow rule is not possible.

That leaves two ways to allowlist:

1. **By file hash** — precise, but the hash changes every release, so
   IT has to update the rule on each version bump. We publish the
   per-release SHA256 list (`meerkat_<v>_checksums.txt`) on every
   release, and `generate-hash-rules.ps1` here turns it into ready-made
   policy snippets.
2. **By path** — one rule that covers all current and future releases
   installed to the standard locations. Weaker (anything at that path
   is trusted), but the standard meerkat install paths are
   **user-profile-scoped** directories that the user already controls,
   so the additional exposure is small. **This is the approach the
   templates in this folder take**, per the maintainers' request.

If your organization later adopts Authenticode code-signing, these templates
should be replaced with a single publisher rule — see
[Future: publisher-based rules](#future-publisher-based-rules).

## Install paths we allowlist

These are the paths meerkat documents in
[`docs/INSTALL.md`](../../docs/INSTALL.md). The templates allow the
`meerkat` / `mk` binary in each.

### Windows

| Path | Notes |
|---|---|
| `%LOCALAPPDATA%\Programs\meerkat\meerkat.exe` | Primary recommended install dir |
| `%LOCALAPPDATA%\Programs\meerkat\mk.exe` | Convenience alias (copy of meerkat.exe) |
| `%USERPROFILE%\bin\meerkat.exe` | Documented alternative |
| `%USERPROFILE%\bin\mk.exe` | Convenience alias |
| `%ProgramFiles%\meerkat\meerkat.exe` | Admin / machine-wide install (optional) |

`mk update` also briefly writes `meerkat.exe.new` and
`meerkat.exe.old` siblings during a self-update. The path rules use
a wildcard on the directory so those transient names are covered.

### macOS

| Path | Notes |
|---|---|
| `~/.local/bin/meerkat` | Primary recommended install dir |
| `~/.local/bin/mk` | Convenience symlink |
| `/opt/homebrew/bin/meerkat` | Apple Silicon Homebrew prefix (EDR-allowlisted) |
| `/opt/homebrew/bin/mk` | Convenience symlink |
| `/usr/local/bin/meerkat` | Intel Homebrew prefix |
| `/usr/local/bin/mk` | Convenience symlink |

## Files in this folder

| File | Engine | Platform | Notes |
|---|---|---|---|
| `wdac-meerkat-path-policy.xml` | WDAC (Windows Defender Application Control) | Windows | Path-based `Allow` rules. Convert to `.cip` with `ConvertFrom-CIPolicy` before deployment. |
| `applocker-meerkat-path-policy.xml` | AppLocker | Windows | Path-based `Allow` rules for the EXE collection. Import via Group Policy or `Set-AppLockerPolicy`. |
| `defender-macos-exclusions.md` | Defender for Endpoint (macOS) | macOS | `mdatp` exclusion commands + MDM (Intune/Jamf) `managed.json` snippet. |
| `generate-hash-rules.ps1` | (any) | Windows/macOS | Optional helper: fetches a release's checksums and emits hash-based rule snippets, for IT who prefer hash rules over path rules. |

## Deploying the Windows templates

### WDAC

```powershell
# 1. (optional) review / edit the policy XML
notepad .\wdac-meerkat-path-policy.xml

# 2. compile to binary policy
ConvertFrom-CIPolicy `
  -XmlFilePath .\wdac-meerkat-path-policy.xml `
  -BinaryFilePath .\wdac-meerkat-path-policy.cip

# 3. deploy (test in audit mode first — see the <!-- AUDIT --> note in the XML)
#    via MDM/Intune (recommended) or locally for testing:
Copy-Item .\wdac-meerkat-path-policy.cip `
  "$env:windir\System32\CodeIntegrity\CIPolicies\Active\{POLICY-GUID}.cip"
# then refresh:
Invoke-CimMethod -Namespace root\Microsoft\Windows\CI `
  -ClassName PS_UpdateAndCompareCIPolicy -MethodName Update `
  -Arguments @{ FilePath = "...\{POLICY-GUID}.cip" }
```

WDAC is a base/supplemental policy system. If IT already runs a base
WDAC policy, deploy this as a **supplemental** policy (the template's
`PolicyType` and `BasePolicyID` are commented with guidance).

### AppLocker

```powershell
# Merge into existing AppLocker policy, or set directly (test first):
Set-AppLockerPolicy -XmlPolicy .\applocker-meerkat-path-policy.xml -Merge

# Verify:
Get-AppLockerPolicy -Effective -Xml
```

AppLocker requires the **Application Identity** service (`AppIDSvc`)
running. The template targets the EXE rule collection.

## Deploying the macOS template

See [`defender-macos-exclusions.md`](./defender-macos-exclusions.md).
macOS Defender doesn't do WDAC/AppLocker-style allowlisting; it uses
`mdatp` exclusions (path/file) which can be pushed via Intune or Jamf
as a `managed.json` configuration profile.

## A note on macOS "code 137 / SIGKILL" symptoms

If `meerkat` exits with code 137 and no output on a managed Mac, that
is the EDR (Defender or another agent) killing exec from a
non-allowlisted path. The fix is exactly what the macOS template
allowlists: run meerkat from `~/.local/bin`, `/opt/homebrew/bin`, or
`/usr/local/bin`, and add those paths to the Defender exclusions. The
binary content is identical regardless of path — only the path
matters to the policy. See the troubleshooting table in
[`docs/INSTALL.md`](../../docs/INSTALL.md).

## Future: publisher-based rules

If your organization procures a code-signing certificate and starts
Authenticode-signing `meerkat.exe` in CI, replace the path-based
Windows rules here with a single **publisher** rule:

- WDAC: a `Signer` / `FileRules` allow keyed on the certificate's
  subject + the meerkat product name.
- AppLocker: a `FilePublisherRule` keyed on `O=YOUR-ORG..., PRODUCT=MEERKAT`.

That collapses "a rule per release" or "a path we broadly trust" into
"any binary we actually signed, anywhere" — strictly better. Until
then, path-based is the pragmatic choice.
