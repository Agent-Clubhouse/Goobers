$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. (Join-Path $PSScriptRoot 'Release-Metadata.ps1')
$dependencies = Get-Content -LiteralPath dependencies.json -Raw | ConvertFrom-Json
foreach ($name in @('mingit', 'zoneinfo')) {
    $actual = (Get-FileHash -LiteralPath "$name.zip" -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $dependencies.$name.sha256) { throw "$name.zip SHA256 mismatch" }
}
# Only these release-engine inputs are copied into the runtime image. Require
# their checksum entries, rejecting duplicate/ambiguous records before extraction.
$lines = @(Get-Content -LiteralPath SHA256SUMS)
foreach ($name in @('goobers.exe', 'goobers-operator.exe', 'release.json')) {
    $pattern = '^([a-fA-F0-9]{64})  ' + [regex]::Escape($name) + '$'
    $entries = @($lines | Where-Object { $_ -match $pattern })
    if ($entries.Count -ne 1) { throw "Expected one checksum for $name" }
    $expected = $entries[0].Substring(0, 64)
    if ((Get-FileHash -LiteralPath $name -Algorithm SHA256).Hash -ne $expected) { throw "$name SHA256 mismatch" }
}
$release = Read-ImageRelease -Path (Join-Path (Get-Location) 'release.json')
if ($release.schemaVersion -ne 1 -or $release.kind -ne 'goobers-base-build-inputs' -or $release.platform -ne 'windows/amd64') {
    throw 'Expected release-engine schema 1 windows/amd64 base inputs'
}
if ($release.commit -notmatch '^[a-f0-9]+$' -or [string]::IsNullOrWhiteSpace($release.version) -or [string]::IsNullOrWhiteSpace($release.date)) {
    throw 'Release version, hexadecimal commit stamp and date are required'
}
