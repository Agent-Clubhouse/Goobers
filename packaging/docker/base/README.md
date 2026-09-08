# Azure Linux base-image build inputs

The release engine can prepare base-image contexts from the same binary
bytes, platform, version, commit, and date used for release archives:

```sh
make portal-build
go run ./release -version v0.4.0-rc.1 -commit "$(git rev-parse HEAD)" \
  -date "$(git show -s --format=%cI HEAD)" \
  -previous-features /path/to/previous/feature-registry.json \
  -previous-support-matrix /path/to/previous/dsl-support-matrix.json \
  -targets linux/amd64,linux/arm64 -output dist/rc-assets \
  -image-contexts dist/rc-image-contexts
```

The image-context destination must not already exist. Explicit targets are
required: `linux/amd64`, `linux/arm64`, and `windows/amd64` are accepted, including
mixed batches. `-skip-unbuildable` is rejected. Every requested context appears
together only after the release run succeeds. A failure removes the staging
directory; ordinary release assets follow the existing release behavior.

Each `linux-amd64` or `linux-arm64` context contains `goobers` (copied from the
archive input before cleanup), `goobers-operator` (built with the same platform and
linker metadata), this Dockerfile, `release.json`, and `SHA256SUMS`. The context
checksum manifest is always emitted, including when archive checksums are disabled.
It covers both executables, the Dockerfile, and metadata. Verify it before building:

```sh
cd dist/rc-image-contexts/linux-amd64
sha256sum -c SHA256SUMS
docker build --platform linux/amd64 -t goobers-base:local-rc-check .
docker run --rm --read-only --tmpfs /tmp --tmpfs /home/nonroot:uid=65532,gid=65532 \
  --entrypoint sh goobers-base:local-rc-check \
  -ec 'test "$(id -u)" = 65532; test -w "$HOME"; git --version; goobers --version; goobers-operator --version'
```

Use `linux/arm64` with the corresponding context. The explicit platform prevents
mixing release executables with runtime files for another architecture. The build
runs both executables as uid:gid 65532:65532 and checks required runtime materials.
The final filesystem contains Azure Linux runtime packages, both Goobers binaries,
and release metadata. It omits harness runtimes and package caches and removes
setuid/setgid helpers. `/dev/null` exists only temporarily during package setup;
the container runtime supplies the final `/dev` devices.

For Windows, use `-targets windows/amd64` (or include it in a mixed batch). The
`windows-amd64` context contains archive-matched `.exe` binaries and the same
metadata, plus the committed Windows Dockerfile, verification scripts, and pinned
dependency manifest. Preparation downloads the two public ZIPs over HTTPS,
verifies their SHA256 pins, and covers all context files in `SHA256SUMS`. It requires
a hexadecimal embedded commit stamp; both abbreviated and full stamps are accepted.
Downloads have a two-minute limit each, a 128 MiB MinGit limit and a 4 MiB timezone
archive limit. A failed download discards the entire batch. Windows preparation
runs on any release host; building or running the image still requires a compatible
Windows Server 2022 container host. See the [Windows input and native-check
contract](../windows/README.md).

## Build and verify local images with the release engine

Add `-build-images -image-prefix goobers-rc-local` to the release command to build
images from its prepared inputs. Use only the Docker engine's native target in
that invocation, for example `-targets linux/arm64` on an ARM64 Linux engine.
Preparation without `-build-images` still supports cross-platform and mixed
batches. Ordinary release packaging does not contact Docker.

Linux builds all three families: `goobers-base`, `goobers-harness-copilot`, and
`goobers-harness-claude`. A native Windows engine builds `goobers-base-windows`;
Windows harness publishing scope remains undecided. Tags take the form
`<image-prefix>/<family>:<version>-<commit>-<os>-<arch>`. The prefix can include
a registry/repository path, but these tags stay local: this command does not push,
sign, or create a published multi-architecture manifest. Existing local tags are
refused before release work begins.

The engine verifies both binary stamps and SHA256 hashes inside each image,
against the exact release inputs. Linux probes run as UID/GID 65532 with a
read-only root, no network, no capabilities, no privilege escalation, and noexec
temporary HOME and `/tmp`. Harness layers bind to the inspected base image ID
through an owned temporary alias; the engine checks that binding, inherited base
layers, binary bytes and runtime versions, then removes the alias. Windows probes
require `ContainerUser` and omit unsupported Linux filesystem flags.

`image-evidence.json` appears inside the finalized context root and records the
expected release metadata, native Docker platform, immutable image IDs, local
tags, context checksums, binary hashes, and CLI version output. Harness contexts
are retained beside the platform context with their pinned inputs and tree
checksums. Failed image builds or checks remove the entire context batch. Created
local image tags may remain for inspection and are named in the error; the engine
does not overwrite or delete existing user images.

These are **local preparation and verification**, bounded parts of
[#3275](../../../docs/design/goobernetes-deployment-images.md). It does not publish
or sign images, create registry tags or multi-architecture manifests, promote
Windows support, or establish the distributed smoke exit. Both Linux architectures
are accepted as build inputs; official multi-architecture publishing remains a
separate decision. The Azure Linux builder is digest-pinned, while RPM packages
come from its configured repositories; package snapshot pinning, vulnerability
scanning, provenance/signing, compressed-size enforcement, authenticated harness stages,
published Windows images, and publish-time native smoke remain release work.
