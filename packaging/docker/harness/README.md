# Linux harness layers

This Dockerfile adds exactly one harness to the Azure Linux release base. The
native upstream packages include their own runtime; no separate Node installation
or second Goobers build is needed. All package files, including licenses and
bundled native modules, remain in the image. The Go release helper in
`release/internal/imageinputs` pins each architecture's public npm archive with
SHA-512 and verifies it before bounded extraction. It refuses traversal paths,
links, devices, duplicate files, and privileged file modes, and publishes the
prepared context only after the complete operation succeeds.
Executable-entry validation uses the archive's Unix permission metadata. Docker
then sets all immutable package assets and launchers to mode `0555`, so Windows
clients do not lose executable bits when sending a context to a Linux engine.

The release engine prepares the package, launcher, version, source provenance,
and Dockerfile together. The repository directory alone is not a build context:
no Docker build downloads or executes a project-authored preparation script.
To rebuild a prepared context, select the exact base from that release. Use a
digest reference for retained builds; a local tag is useful for unpublished
image testing:

```sh
docker build --platform linux/arm64 \
  --build-arg GOOBERS_BASE_IMAGE=goobers-base:local-rc-check \
  --build-arg HARNESS=copilot \
  -t goobers-harness-copilot:local-rc-check /path/to/prepared-copilot-context
docker build --platform linux/arm64 \
  --build-arg GOOBERS_BASE_IMAGE=goobers-base:local-rc-check \
  --build-arg HARNESS=claude \
  -t goobers-harness-claude:local-rc-check /path/to/prepared-claude-context
```

`linux/amd64` has separate pinned upstream archives. Other architectures and
harness names fail before downloading. The build checks the harness's version
as UID 65532. The base supplies the Goobers and operator binaries unchanged,
along with its entrypoint, default command, writable home, git, CA roots, and
time zone data. The downloaded archive remains outside the final image; package preparation
uses the Go release tooling and adds no package manager or curl layer.

Run the stricter runtime check with a read-only root and **noexec** temporary
home; change the image name and command together for Claude:

```sh
docker run --rm --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges \
  --tmpfs /tmp:noexec --tmpfs /home/nonroot:noexec,uid=65532,gid=65532 \
  --entrypoint sh goobers-harness-copilot:local-rc-check \
  -ec 'test "$(id -u)" = 65532; git --version; goobers --version; goobers-operator --version; copilot --version'
```

The Copilot launcher sets `COPILOT_CLI_DIST_DIR` to the immutable package
directory. Without it, the native loader extracts executable addons into the
home cache and fails on a noexec home. Both launchers disable automatic updates
so the pinned build remains the executed build. They set these fixed values
themselves, since a stage's default-deny environment can remove ambient image
variables. Runtime configuration, provider authentication, and session state
still belong in the invocation's writable home/workspace and are never image
inputs.

Pins were resolved from the public npm manifests for
[`@github/copilot`](https://registry.npmjs.org/@github/copilot/1.0.80) and
[`@anthropic-ai/claude-code`](https://registry.npmjs.org/@anthropic-ai/claude-code/2.1.263)
and their exact optional platform packages. Updating a pin requires checking the
new package's native runtime and launcher behavior, repeating the restricted
runtime test, and recording fresh image evidence.

These recipes prepare the two Linux families required by
[the image contract](../../../docs/design/goobernetes-deployment-images.md).
Release-engine image build/sign/push integration, published image availability,
native AMD64 checks, authenticated agentic stages, and distributed S1–S9 evidence
are still required before promotion. Windows harness publishing remains the
design's open scope decision; this Dockerfile does not target Windows.
