# Windows Server 2022 base-image preparation

This is a prepared-input Dockerfile for the `goobers-base` family in
[the approved image contract](../../../docs/design/goobernetes-deployment-images.md).
It has **not** been built or executed on a Windows container host. It does not
publish an image, close #3275, or promote Windows support. The release engine's
`-image-contexts` path currently prepares Linux only; Windows context generation,
signing and publication remain pending.

The runtime uses Server Core because its Windows API surface and built-in
PowerShell support the MSYS2-based POSIX shell and offline ZIP/certificate setup.
Nano Server lacks PowerShell and has a smaller API surface; rebasing needs native
compatibility evidence. See [Microsoft's base-image guidance](https://learn.microsoft.com/en-us/virtualization/windowscontainers/manage-containers/container-base-images).

## Inputs and pins

`dependencies.json` records the exact external materials. As verified on 2026-09-07:

| Material | Pin / source |
| --- | --- |
| Windows Server Core 2022, windows/amd64 | `mcr.microsoft.com/windows/servercore:ltsc2022@sha256:6b43c814ed2a800563083ce3193e5f1951d4d6a18fd2879ff45173851db82bd5`, manifest OS version `10.0.20348.5499`; resolved from [MCR manifest metadata](https://mcr.microsoft.com/v2/windows/servercore/manifests/ltsc2022) |
| Regular MinGit 2.55.0.5, x64 | `MinGit-2.55.0.5-64-bit.zip`, SHA256 `56d7b226b7693196cfc71fef26568f536c4a021ab6c37ff2db4287bed908e96e`; matches [the upstream release checksum](https://github.com/git-for-windows/git/releases/tag/v2.55.0.windows.5) and the downloaded archive |
| Go 1.26.6 IANA timezone archive | `lib/time/zoneinfo.zip`, SHA256 `8f55634d05f8bca1f7bc7c69c5933428c69357e0bdf565e5ba224e3f88ff12e8`; verified against [the Go release source](https://github.com/golang/go/blob/go1.26.6/lib/time/zoneinfo.zip) |

The regular MinGit ZIP contains `cmd/git.exe`, `usr/bin/sh.exe`, the MSYS2
runtime, licenses, and `mingw64/etc/ssl/certs/ca-bundle.crt`. It excludes the full
interactive Git Bash distribution; this image claims POSIX `sh`, not Bash.
The separate BusyBox variant is deliberately excluded. These distinctions follow
[MinGit's upstream contract](https://gitforwindows.org/mingit).

The bundled public CA roots are imported into the Windows machine trust store
for Go's Windows TLS verifier; Git uses the same PEM bundle via OpenSSL. No private
CA, token, kubeconfig, credentials, instance config, or user Git configuration
belongs in this input directory. The Git distribution's installer defaults for
credential manager, LFS and host Git-config includes are replaced. `ZONEINFO`
points Go to the supplied archive, using [Go's documented lookup order](https://pkg.go.dev/time#LoadLocation).

Refresh the base digest with Windows security servicing, and MinGit with upstream
security releases; review their release notes and rerun native checks. Refresh the
zoneinfo source and checksum when the repository Go toolchain changes. Both the
Dockerfile and manifest must change together for a base update. A build-argument
base override is available for deliberate testing, but requires equivalent source
review and native evidence before publication.

## Prepare an isolated build context

Use an empty staging directory such as `C:\image-contexts\windows-amd64`, never a
checkout or instance root. Stage these files from one release build:

- `goobers.exe`, built with embedded Portal assets and the release version, full
  40-character commit and build date;
- `goobers-operator.exe`, built for `windows/amd64` with **the same three ldflags**;
- `release.json`, the same metadata shape the Linux release inputs use, with
  `platform` changed to `windows/amd64`;
- `SHA256SUMS`, containing exactly one checksum entry for each of those three files
  in the release engine's `<sha256><two spaces><filename>` format.

For example, metadata has this shape (replace all example values with the release
build's real values; no placeholders pass the native stamp check):

```json
{
  "schemaVersion": 1,
  "kind": "goobers-base-build-inputs",
  "version": "v0.4.0-rc.1",
  "commit": "0123456789abcdef0123456789abcdef01234567",
  "date": "2026-09-07T00:00:00Z",
  "platform": "windows/amd64"
}
```

The existing release archives can supply the signed `goobers.exe`. Until Windows
input generation is wired into the release engine, the operator must be prepared
from the exact same source using its existing Go release build settings
(`GOOS=windows`, `GOARCH=amd64`, `CGO_ENABLED=0`, `-trimpath`, `-ldflags` setting
`internal/version.Version`, `Commit`, and `Date`). Keep the resulting checksums
with that release's evidence. This manual bridge is preparation, not an official
publication path; the image's native checks reject mismatched binary stamps.

Copy `Dockerfile`, `.dockerignore`, `dependencies.json`, `Verify-Inputs.ps1`, `Configure-Image.ps1`,
`Verify-Image.ps1`, and `Shell-Contract.sh` from this directory into the context. Download the two ZIPs
using the manifest URLs, without authentication. On a trusted preparation host,
from inside the context:

```powershell
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$dependencies = Get-Content dependencies.json -Raw | ConvertFrom-Json
foreach ($name in @('mingit', 'zoneinfo')) {
    Invoke-WebRequest -UseBasicParsing -Uri $dependencies.$name.url -OutFile "$name.zip"
    $hash = (Get-FileHash "$name.zip" -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($hash -ne $dependencies.$name.sha256) { throw "$name SHA256 mismatch" }
}
powershell -NoProfile -NonInteractive -File .\Verify-Inputs.ps1
```

Only the base-image pull requires registry access during the Docker build. The
Dockerfile downloads no packages and verifies the supplied ZIP and release-file
checksums before extraction. Its explicit COPY list does not include arbitrary
context contents, and `.dockerignore` excludes files outside that list from the
builder context. ZIPs and the checksum verifier stay in the intermediate stage.

## Native gates still required

On a compatible Windows container builder, build a local commit-tagged image:

```powershell
docker build --isolation=process -t goobers-base:<full-commit>-windows-amd64 .
docker run --rm --entrypoint powershell goobers-base:<full-commit>-windows-amd64 -NoProfile -NonInteractive -File C:\Goobers\Verify-Image.ps1
```

Use a host compatible with the pinned OS image; see [Microsoft's host/image
compatibility matrix](https://learn.microsoft.com/en-us/virtualization/windowscontainers/deploy-containers/version-compatibility).
The Dockerfile's final build step runs the same verification as `ContainerUser`:
Git and shell on PATH, both binary stamps, timezone entries, and Git workspace /
HOME / temp writes. Administrative setup is confined to the build stage before
`USER ContainerUser`.

Before a release claim, additionally measure and retain:

1. Native image build, inspect output, OS build compatibility and binary stamps;
   tampered binary/dependency inputs must fail the build.
2. Go timezone loading through a real daemon configuration, Windows TLS and Git
   HTTPS against public endpoints, and negative TLS controls with an untrusted CA.
3. Dispatcher-created stage execution as ContainerUser with mounted workspace,
   HOME and temp roots; a consumer image layer and direct `goobers` command override.
   Windows does not support Kubernetes `readOnlyRootFilesystem`; enforce/test the
   applicable Windows restriction backend instead of claiming a Linux mount flag.
4. Required cross-node / cross-OS smoke evidence, Windows harness-family publishing
   scope, and release-engine Windows context/build/sign/publish integration.

No native checks above have been run by the preparation change. Do not infer a
support-matrix promotion from the presence of this Dockerfile.
