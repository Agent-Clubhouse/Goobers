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

This is **build-input preparation**, a bounded part of
[#3275](../../../docs/design/goobernetes-deployment-images.md). It does not publish
or sign images, create registry tags or multi-architecture manifests, promote
Windows support, or establish the distributed smoke exit. Both Linux architectures
are accepted as build inputs; official multi-architecture publishing remains a
separate decision. The Azure Linux builder is digest-pinned, while RPM packages
come from its configured repositories; package snapshot pinning, vulnerability
scanning, provenance/signing, compressed-size enforcement, harness image layers,
Windows images, and publish-time native smoke remain release work.
