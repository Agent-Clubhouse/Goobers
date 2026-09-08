# Windows Server 2022 base-image preparation

This is a prepared-input Dockerfile for the `goobers-base` family in
[the approved image contract](../../../docs/design/goobernetes-deployment-images.md).
It has **not** been built or executed on a Windows container host. It does not
publish an image, close #3275, or promote Windows support. The release engine's
`-image-contexts` path prepares Windows and Linux inputs; native image validation,
signing and publication remain release gates.

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

The release engine stages Windows inputs in a new output root. From a checkout
with embedded Portal assets built, use explicit `windows/amd64` targets:

```powershell
npm --prefix portal ci --no-audit --no-fund
npm --prefix portal run build
go run ./release -version v0.0.0-windows-verify -targets windows/amd64 -output C:\release-assets -image-contexts C:\image-contexts -first-feature-snapshot
```

`-first-feature-snapshot` is appropriate for this standalone verification build;
an official release supplies its normal previous-feature/support baselines.
`C:\image-contexts` must not already exist. The engine produces its
`windows-amd64` child atomically, containing:

- `goobers.exe`, byte-identical to the generated release archive, and
  `goobers-operator.exe`, with the same version, commit and build-date stamps;
- `release.json`, schema 1 `goobers-base-build-inputs` metadata with
  `platform: windows/amd64`;
- the committed Dockerfile, `.dockerignore`, dependency manifest, setup and
  verification scripts;
- checksum-verified `mingit.zip` and `zoneinfo.zip`, fetched over HTTPS from the
  pinned manifest sources;
- `SHA256SUMS` for all other context files, in the release engine's
  `<sha256><two spaces><filename>` format.

The metadata commit must equal the binary's stamp: abbreviated hashes from
existing release artifacts are accepted. Image SHA tags may independently use
the full 40-character source revision. The build date matches literally,
preserving `Z`, offsets and fractional seconds across PowerShell versions; it
is not converted to a local DateTime. Optional `-commit` and `-date` flags use
the same release-engine inputs for both binaries and metadata.

The engine downloads dependencies during preparation; Docker build does not
fetch them. Do not use a checkout or instance root as the Docker build context.
This path prepares unsigned build inputs. Official signing/publication ordering
and matching the final signed Windows archive to its image remain pending.

Only the base-image pull requires registry access during the Docker build. The
Dockerfile downloads no packages and verifies the supplied ZIP and release-file
checksums before extraction. Its explicit COPY list does not include arbitrary
context contents, and `.dockerignore` excludes files outside that list from the
builder context. ZIPs and the checksum verifier stay in the intermediate stage.

## Native CI and manual verification

The existing required Windows CI lane runs `TestWindowsImage` under explicit
Windows PowerShell 5.1. `GOOBERS_REQUIRE_WINDOWS_POWERSHELL=1` makes an unavailable
or different runtime fail the tests; it cannot silently skip or substitute
PowerShell 7. This covers checksum rejection, exact date/stamp comparison and
script parsing. It does not execute Docker, certificate-store setup or the
ContainerUser runtime.

The opt-in [Windows image workflow](../../../.github/workflows/windows-image-verify.yml)
uses `workflow_dispatch` and a `windows-2022` host. It prepares the release inputs,
checks archive checksums and exact binary parity, builds the local Windows image,
then verifies both image binaries and runs quickstart plus the shipped mock demo
as ContainerUser. It uploads logs and image/container inspection evidence even
when verification fails, and never pushes an image. The demo allows unisolated
`network: none` execution for this trusted, credential-free mock workload;
this is not proof of Windows network enforcement.

A manually dispatched workflow must first exist on the default branch. The
required Windows CI lane can provide PowerShell evidence on a PR before that.
Neither workflow has been executed by this preparation change. Review the actual
run artifacts before making a native compatibility claim.

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
   scope, and release-engine Windows signing/publication integration.

No native checks above have been run by the preparation change. Do not infer a
support-matrix promotion from the presence of this Dockerfile.
