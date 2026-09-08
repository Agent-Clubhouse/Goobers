$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. (Join-Path $PSScriptRoot 'Release-Metadata.ps1')
$identity = [System.Security.Principal.WindowsIdentity]::GetCurrent()
if ($identity.User.Value -ne 'S-1-5-93-2-2') { throw "Expected ContainerUser, got $($identity.Name)" }
$release = Read-ImageRelease -Path C:\Goobers\release.json
foreach ($binary in @('goobers', 'goobers-operator')) {
    # Operator writes version to stderr. Merge in cmd.exe, before Windows
    # PowerShell 5.1 can turn native stderr into a terminating ErrorRecord.
    $output = (& cmd.exe /d /c "$binary --version 2>&1" | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw "$binary --version failed: $output" }
    Assert-ImageReleaseStamp -Release $release -Output $output -Binary $binary
}
& git --version
if ($LASTEXITCODE -ne 0) { throw 'git unavailable on PATH' }
# Exercise the POSIX shell builtin without nested native argument quoting.
& sh -c 'exit 0'
if ($LASTEXITCODE -ne 0) { throw 'POSIX shell contract failed' }
Add-Type -AssemblyName System.IO.Compression.FileSystem
$zones = [System.IO.Compression.ZipFile]::OpenRead($env:ZONEINFO)
try {
    foreach ($name in @('UTC', 'America/Los_Angeles', 'Europe/London')) {
        if ($null -eq $zones.GetEntry($name)) { throw "Missing IANA timezone $name" }
    }
} finally { $zones.Dispose() }
# Exercise git in a workspace selected at runtime; no writes under C:\Git or
# C:\Goobers. A consumer may rerun this script with its mounted workspace as cwd.
$probe = Join-Path (Get-Location) ('image-contract-' + [Guid]::NewGuid().ToString('N'))
try {
    & git init --quiet $probe
    if ($LASTEXITCODE -ne 0) { throw 'git could not initialize workspace' }
    Set-Content -LiteralPath (Join-Path $probe 'probe.txt') -Value 'image contract'
    & git -C $probe add probe.txt
    if ($LASTEXITCODE -ne 0) { throw 'git could not stage workspace data' }
    foreach ($path in @($env:HOME, $env:TEMP)) {
        $file = Join-Path $path ('image-contract-' + [Guid]::NewGuid().ToString('N'))
        Set-Content -LiteralPath $file -Value 'writable'
        Remove-Item -LiteralPath $file
    }
} finally {
    if (Test-Path -LiteralPath $probe) { Remove-Item -LiteralPath $probe -Recurse -Force }
}
