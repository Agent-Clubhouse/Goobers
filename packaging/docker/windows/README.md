# Windows Server 2022 base-image preparation

This is a prepared-input Dockerfile for the `goobers-base` family in
[the approved image contract](../../../docs/design/goobernetes-deployment-images.md).
Its presence alone does not publish an image, close #3275, or promote Windows
support. The release engine's `-image-contexts` path prepares Windows and Linux
inputs. Native Windows builds now run both in an optional prepublication workflow
and as a mandatory gate in the tagged-release workflow; neither path publishes a
container image or changes the Windows support tier.

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
gh release download v0.3.3 --pattern feature-registry.json --pattern dsl-support-matrix.json --dir C:\release-baseline
go run ./release -version v0.4.0-rc.1 -targets windows/amd64 -output C:\release-assets -image-contexts C:\image-contexts -previous-features C:\release-baseline\feature-registry.json -previous-support-matrix C:\release-baseline\dsl-support-matrix.json
```

The candidate is checked against the real DSL support policy and previous
published feature/support snapshots. A synthetic older release version or an
empty baseline would bypass or fail the policy this verification must exercise.
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
This manual path prepares unsigned build inputs. The tagged-release workflow
separately signs `goobers.exe`, imports the final signed archive into the image
context, and verifies a native image built from those exact inputs before GitHub
release publication can proceed.

Only the base-image pull requires registry access during the Docker build. The
Dockerfile downloads no packages and verifies the supplied ZIP and release-file
checksums before extraction. Its explicit COPY list does not include arbitrary
context contents, and `.dockerignore` excludes files outside that list from the
builder context. ZIPs and the checksum verifier stay in the intermediate stage.

## Native CI, optional precheck, and the release gate

The existing required Windows CI lane runs `TestIntegrationWindowsImage*` with
`-tags=integration` under explicit Windows PowerShell 5.1. These Windows-only
integration tests declare `powershell.exe` through `testdep.Require`;
`TESTDEP_STRICT=1` makes a missing executable fail, and the runtime check refuses
PowerShell 7. Ordinary unit tests execute no external PowerShell process. This covers checksum rejection, exact date/stamp comparison and
script parsing. It does not execute Docker, certificate-store setup or the
ContainerUser runtime.

The opt-in [Windows image workflow](../../../.github/workflows/windows-image-verify.yml)
uses `workflow_dispatch` and a `windows-2022` host. Its candidate defaults to
`v0.4.0-rc.1` and published baseline to `v0.3.3`, both explicit dispatch inputs.
It downloads the baseline snapshots with a read-only GitHub token and invokes
`go run ./release` with `-build-images -image-prefix` using a unique local prefix
for the run. The release engine builds the image and verifies its native runtime
and binary hashes. The workflow requires `image-evidence.json` to identify exactly
one native Windows base image, then checks archive parity and runs quickstart
plus the shipped mock demo as ContainerUser using that same image. It retains
the engine evidence and checks the image ID before the additional smoke. It uploads logs and image/container inspection evidence even
when verification fails, and never pushes an image. The demo allows unisolated
`network: none` execution for this trusted, credential-free mock workload;
this is not proof of Windows network enforcement.

This optional precheck first completed successfully on 2026-09-12 for
`candidate_version=v0.4.0-rc.2` and `baseline_tag=v0.3.3`; its retained evidence
is attached to [workflow run 34679030863](https://github.com/Agent-Clubhouse/Goobers/actions/runs/34679030863).
It remains a manually requested early check, not a prerequisite that the tagged
release workflow consumes. Whether maintainers should require, automate, or
retire this extra pre-tag check remains tracked in #4895.

Every release tag independently runs the mandatory `native-windows-image` job in
[`release.yml`](../../../.github/workflows/release.yml). That job consumes the
final Authenticode-signed artifact by its exact artifact ID, rebuilds the Windows
image through the release engine on `windows-2022`, requires native
`windows/amd64` evidence for exactly one `goobers-base-windows` image, and retains
the provenance. Both release validation and `verify-and-publish` depend on this
job, so a failure prevents GitHub release publication. This is the authoritative
publication gate; the optional workflow supplies earlier, additional evidence.

## Additional native evidence for support claims

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

The tagged-release gate proves the image can be built natively from the final
signed release inputs and satisfies the release engine's verification. It does
not by itself establish every runtime or deployment property needed for a
broader Windows support claim. Before such a claim, additionally measure and
retain:

1. Native image build, inspect output, OS build compatibility and binary stamps;
   tampered binary/dependency inputs must fail the build.
2. Go timezone loading through a real daemon configuration, Windows TLS and Git
   HTTPS against public endpoints, and negative TLS controls with an untrusted CA.
3. Dispatcher-created stage execution as ContainerUser with mounted workspace,
   HOME and temp roots; a consumer image layer and direct `goobers` command override.
   Windows does not support Kubernetes `readOnlyRootFilesystem`; enforce/test the
   applicable Windows restriction backend instead of claiming a Linux mount flag.
4. Required cross-node / cross-OS smoke evidence and Windows harness-family
   publishing scope.

Do not infer a support-matrix promotion from the presence of this Dockerfile or
from either native image workflow.
