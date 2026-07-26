#requires -Version 5.1
<#
.SYNOPSIS
  Generate hash-based Defender allowlist snippets for a meerkat release.

.DESCRIPTION
  Path-based rules (the other templates in this folder) cover all
  releases but trust a whole directory. If Internal-IT prefers
  precise per-release HASH rules instead, this script fetches a
  release's published SHA256 checksums file from the GitHub release
  assets and emits:

    - a WDAC <Allow Hash="..."> FileRules fragment
    - an AppLocker <FileHashRule> fragment
    - a plain "<sha256>  <filename>" list (for mdatp / manual use)

  meerkat is NOT Authenticode-signed, so publisher rules are not an
  option; hash rules are the precise-but-recurring alternative to the
  path rules. Re-run this on every release bump.

.PARAMETER Version
  Release version WITHOUT the leading 'v' (e.g. 0.6.0).

.PARAMETER Token
  GitHub token. Defaults to $env:GITHUB_TOKEN, then the cached gh
  OAuth token (gh auth token). Only needed while the repo is private.

.PARAMETER Owner
  GitHub repository owner. Defaults to 'your-org'.

.PARAMETER Repo
  GitHub repository name. Defaults to 'meerkat'.

.PARAMETER OutDir
  Where to write the generated fragments. Defaults to the current dir.

.EXAMPLE
  .\generate-hash-rules.ps1 -Version 0.6.0
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)]
  [string]$Version,

  [string]$Token,

  [string]$Owner = 'your-org',

  [string]$Repo = 'meerkat',

  [string]$OutDir = "."
)

$ErrorActionPreference = 'Stop'

function Resolve-Token {
  if ($Token) { return $Token }
  if ($env:GITHUB_TOKEN) { return $env:GITHUB_TOKEN }

  if (Get-Command gh -ErrorAction SilentlyContinue) {
    $t = (& gh auth token 2>$null)
    if ($LASTEXITCODE -eq 0 -and $t) { return $t.Trim() }
  }
  throw "No token: pass -Token, set `$env:GITHUB_TOKEN, or run 'gh auth login'."
}

$tok = Resolve-Token
$tag = "v$Version"
$apiHeaders = @{
  Authorization = "Bearer $tok"
  'User-Agent'  = 'meerkat-hash-rules'
  Accept        = 'application/vnd.github+json'
}

Write-Host "Fetching release $tag from $Owner/$Repo"
$release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Owner/$Repo/releases/tags/$tag" -Headers $apiHeaders
$asset = $release.assets | Where-Object { $_.name -eq "meerkat_${Version}_checksums.txt" } | Select-Object -First 1
if (-not $asset) { throw "checksums asset 'meerkat_${Version}_checksums.txt' not found in release $tag" }

# The asset API URL serves the bytes when Accept is octet-stream.
$checksums = (Invoke-WebRequest -Uri $asset.url -Headers @{
    Authorization = "Bearer $tok"
    'User-Agent'  = 'meerkat-hash-rules'
    Accept        = 'application/octet-stream'
  }).Content

# Parse "hash  filename" lines. Keep only the windows .zip and the
# raw exe-bearing artifacts that IT would actually allow. We allow
# IT to rule on the archives by hash; for EXE-level hash rules they
# should extract and hash meerkat.exe (documented below).
$rows = foreach ($line in ($checksums -split "`n")) {
  if ($line -match '^\s*([0-9a-fA-F]{64})\s+(\S+)\s*$') {
    [pscustomobject]@{ Sha256 = $Matches[1].ToLower(); File = $Matches[2] }
  }
}

if (-not $rows) { throw "No checksum rows parsed — is the file format unexpected?" }

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

# 1. plain list
$plainPath = Join-Path $OutDir "meerkat_${Version}_sha256.txt"
$rows | ForEach-Object { "{0}  {1}" -f $_.Sha256, $_.File } | Set-Content -Path $plainPath -Encoding ascii
Write-Host "Wrote $plainPath"

# 2. WDAC FileRules fragment (hash rules apply to the archive hashes;
#    for EXE-level rules, hash the extracted meerkat.exe and add a row)
$wdac = @()
$wdac += '<!-- meerkat ' + $Version + ' hash allow rules. Paste <Allow> entries into <FileRules>'
$wdac += '     and matching <FileRuleRef> into the user-mode SigningScenario. -->'
$i = 0
foreach ($r in $rows) {
  $id = "ID_ALLOW_MEERKAT_HASH_$i"
  $wdac += ('<Allow ID="{0}" FriendlyName="meerkat {1} {2}" Hash="{3}" />' -f $id, $Version, $r.File, $r.Sha256.ToUpper())
  $i++
}
$wdacPath = Join-Path $OutDir "wdac-meerkat-${Version}-hash-fragment.xml"
$wdac | Set-Content -Path $wdacPath -Encoding utf8
Write-Host "Wrote $wdacPath"

# 3. AppLocker FileHashRule fragment
$al = @()
$al += '<!-- meerkat ' + $Version + ' AppLocker hash rules. Merge into the Exe RuleCollection. -->'
$al += '<RuleCollection Type="Exe" EnforcementMode="AuditOnly">'
$al += '  <FileHashRule Id="' + [guid]::NewGuid() + '" Name="meerkat ' + $Version + ' (hash)"'
$al += '                Description="Allow meerkat release ' + $Version + ' by SHA256" UserOrGroupSid="S-1-1-0" Action="Allow">'
$al += '    <Conditions><FileHashCondition>'
foreach ($r in $rows) {
  # AppLocker wants SHA256 as 0x-prefixed uppercase hex, Type="SHA256"
  $al += ('      <FileHash Type="SHA256" Data="0x{0}" SourceFileName="{1}" SourceFileLength="0" />' -f $r.Sha256.ToUpper(), $r.File)
}
$al += '    </FileHashCondition></Conditions>'
$al += '  </FileHashRule>'
$al += '</RuleCollection>'
$alPath = Join-Path $OutDir "applocker-meerkat-${Version}-hash-fragment.xml"
$al | Set-Content -Path $alPath -Encoding utf8
Write-Host "Wrote $alPath"

Write-Host ""
Write-Host "Done. NOTE: these hash the published ARCHIVES (.zip/.tar.gz)."
Write-Host "For EXE-level rules, extract meerkat.exe from the zip and hash it:"
Write-Host '  Get-FileHash -Algorithm SHA256 .\meerkat.exe'
Write-Host "then add that hash as an extra rule. Re-run this script on every release."
