# CI-only script copied into a stopped test container, never into the base image.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
& C:\Goobers\Verify-Image.ps1
if ($LASTEXITCODE -ne 0) { throw 'Image contract failed' }

& goobers init --allow-ephemeral --template=quickstart C:\workspace\quickstart
if ($LASTEXITCODE -ne 0) { throw 'ContainerUser quickstart initialization failed' }
& goobers validate C:\workspace\quickstart
if ($LASTEXITCODE -ne 0) { throw 'ContainerUser quickstart validation failed' }

& goobers init --allow-ephemeral --demo --insecure C:\workspace\demo
if ($LASTEXITCODE -ne 0) { throw 'ContainerUser mock demo initialization failed' }
# This shipped mock workload has no provider credentials. Its deterministic
# lifecycle is the subject of this smoke, not Windows network isolation proof.
$env:GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE = '1'
& cmd.exe /d /c 'goobers run demo C:\workspace\demo 2>&1' | Tee-Object -FilePath C:\workspace\demo-output.txt
if ($LASTEXITCODE -ne 0) { throw 'ContainerUser mock demo failed' }
if (-not (Select-String -LiteralPath C:\workspace\demo-output.txt -SimpleMatch 'phase=completed' -Quiet)) {
    throw 'Mock demo did not report completed'
}
