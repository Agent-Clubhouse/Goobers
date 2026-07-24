# Releases & packaging

How Goobers binaries are built for distribution, packaged, and verified — and the
Windows distribution story (artifacts, checksums, signing posture, scoop/winget
shape) added by [#655](https://github.com/Agent-Clubhouse/Goobers/issues/655).

## Tagged releases

Pushing a stable semantic-version tag (`vMAJOR.MINOR.PATCH`) runs
`.github/workflows/release.yml`. The workflow builds the packaging engine's
complete matrix, verifies its shared checksum manifest, Linux binary, and
release-pinned documentation, and
creates a GitHub Release containing the archives, `SHA256SUMS`,
`install.sh`, `feature-registry.json`, `dsl-support-matrix.json`, and
`RELEASE_NOTES.md`. The release body and attached notes are the same document:
curated highlights and the commit changelog followed by the DSL feature-support
delta, DSL support-matrix delta, and external-consumer policy. Re-running the
workflow updates the existing release and replaces its assets, so a partially
failed publication can be recovered safely.

Release notes combine a curated overview with the first-parent commit history
since the previous stable tag. Conventional-Commit messages are grouped by type,
including `BREAKING CHANGE:` and `BREAKING-CHANGE:` footers; non-conforming
subjects remain visible under **Other changes**. A non-empty curated overview is
required. Add it at `.github/release-notes/<tag>.md` in the tagged commit, or use
a non-empty annotated-tag message. A lightweight tag without the matching file
fails before publication. The first stable release explicitly starts an empty
feature and DSL-version baselines. Later releases download both JSON snapshots
from the previous stable GitHub Release; a missing prior snapshot stops
publication.

```sh
mkdir -p .github/release-notes
$EDITOR .github/release-notes/v1.2.3.md
git add .github/release-notes/v1.2.3.md
git commit -m "docs: curate v1.2.3 release notes"
git tag v1.2.3
git push origin v1.2.3
```

## Install a pinned release

On Linux or macOS, choose an exact stable release tag and run its attached
installer. Replace `v1.2.3` with the release you intend to adopt:

```sh
VERSION=v1.2.3
/bin/sh -c "$(curl -fsSL "https://github.com/Agent-Clubhouse/Goobers/releases/download/${VERSION}/install.sh")" \
  -- "$VERSION" ./goobers-instance
```

The command downloads only assets attached to that tag. The helper detects the
host OS and architecture, downloads the matching archive plus `SHA256SUMS`,
verifies the archive before extraction, and installs `goobers` to
`$HOME/.local/bin` (override with `GOOBERS_INSTALL_DIR`). It then runs the
release binary's `goobers init --guided` flow, which prompts for the repository,
work tracking, credential references, and canonical workflows and finishes by
validating the generated instance. Use an empty instance path; guided setup
refuses to overwrite existing configuration.

The helper intentionally delegates all config generation and validation to the
installed binary. The canonical workflow templates are embedded in that tagged
binary, so the same tag selects the same checksummed archive and the same
starter config without reading the development repository's moving `main`.
`curl`, `tar`, and either `sha256sum` (Linux) or `shasum` (macOS) are required.
Windows adopters should use the checksum-verified
[Windows release path](quickstart-windows.md).

## The packaging engine

`go run ./release` cross-compiles `./cmd/goobers` for the release matrix,
regenerates the CLI reference, man pages, and completion scripts from that
release's command registry, packages each target with the tagged checkout's
documentation into a platform-conventional archive, and writes a shared
`SHA256SUMS` manifest, the tagged install helper, generated release notes, and
the shipped DSL feature and version-support snapshots into `dist/` (override
with `-output`). It is a standalone Go tool — matching `test/ci` and
`test/coveragegate` — so it runs identically on any release runner without a
shell dependency.

```sh
go run ./release -first-feature-snapshot      # first recorded snapshot only
go run ./release -previous-features previous/feature-registry.json \
  -previous-support-matrix previous/dsl-support-matrix.json
go run ./release -previous-features previous/feature-registry.json \
  -previous-support-matrix previous/dsl-support-matrix.json -targets windows/amd64
go run ./release -previous-features previous/feature-registry.json \
  -previous-support-matrix previous/dsl-support-matrix.json -version v1.2.3 -output dist
```

Build metadata (`version`/`commit`/`date`) is injected via the same
`internal/version` `-ldflags` path the [Makefile](../../Makefile) uses, so a
released binary's `goobers --version` is byte-for-byte consistent with a local
`make build`. Version defaults to `git describe --tags --always --dirty`; the
build date defaults to the commit's committer date, so re-running the engine on
the same commit is reproducible (`-trimpath` is always on).

### Release-pinned onboarding documentation

Every platform archive carries `README.md` and the tagged checkout's complete
`docs/` tree beside the binary. Before packaging, the release engine invokes the
release-stamped binary's hidden documentation generator, which uses the same
registry-backed writer as `make docs` to replace `docs/cli/`, `docs/man/`, and
`docs/completion/`. `docs/RELEASE.md` records the release version and commit.
The tagged workflow then regenerates those three directories with the extracted
binary and diffs them against the archive, so a release cannot publish CLI docs
from another version.

The starter configuration and scaffold templates remain compiled into that same
binary and are emitted by `goobers init`/`goobers scaffold`; release packaging
does not introduce a second template source. Keeping the extracted archive
together therefore gives an installation one release identity across the
binary, onboarding docs, shell completions, man pages, and generated templates.
Start with the bundled `README.md`, use `docs/VISION.md` and
`docs/ARCHITECTURE.md` for the product concepts, then follow
`docs/guides/quickstart.md`.

### Release notes and DSL support snapshots

Every non-empty release build writes three metadata assets alongside the binaries:

- `feature-registry.json` is the complete, schema-versioned snapshot returned by
  the same registry that powers `goobers features` and
  [`docs/feature-matrix.md`](../feature-matrix.md).
- `dsl-support-matrix.json` records the compiled-in DSL `SupportMatrix`.
- `RELEASE_NOTES.md` is rendered from
  [`release/release-notes.tmpl.md`](../../release/release-notes.tmpl.md). It
  includes newly GA, newly deprecated, and removed features; DSL versions newly
  marked `deprecated` or `unsupported` and their `goobers fix --to <version>`
  migration command; and the external-consumer support policy. The tagged
  workflow combines those sections with its required curated overview and
  generated commit changelog, then uses the result as both the attached file and
  the GitHub Release body.

For every release after the first, download both snapshots from the previous
GitHub Release and pass them with `-previous-features` and
`-previous-support-matrix`. The generator validates the snapshots and compares
support levels by stable feature ID and DSL version. A feature must remain in
the registry at level `removed`, not disappear. A newly deprecated or
unsupported DSL version must name a replacement so the release note can provide
a migration path. For the first recorded snapshots, pass
`-first-feature-snapshot` to explicitly select empty baselines; exactly one
baseline mode is required. The
[illustrative generated note](../releases/sample-release-notes.md) shows all
three transition categories.

External consumers should pin the Goobers binary version and both attached
snapshots. Preview features are unstable; GA features carry the compatibility
contract; deprecated features continue to validate with warnings for at least
one released minor before removal; removed features fail validation. Within an
`apiVersion`, optional additions and `preview` to `ga` promotions are
non-breaking. Field removal or renaming, tighter constraints, changed defaults,
and semantic changes require the deprecation window or an `apiVersion` bump.

### Artifact shape

| Target | Archive | Contents |
|---|---|---|
| `windows/amd64` | `goobers_<version>_windows_amd64.zip` | `goobers.exe`, `README.md`, `docs/` |
| `darwin/amd64`, `darwin/arm64` | `goobers_<version>_<os>_<arch>.tar.gz` | `goobers`, `README.md`, `docs/` |
| `linux/amd64`, `linux/arm64` | `goobers_<version>_<os>_<arch>.tar.gz` | `goobers`, `README.md`, `docs/` |

Windows uses `.zip` (the platform convention Windows users and scoop/winget
expect); unix targets use `.tar.gz`.

### Checksums

`SHA256SUMS` is a coreutils `sha256sum -c`-compatible manifest — one
`<hex>  <filename>` line per binary archive, `install.sh`, and the authoritative
`feature-registry.json` and `dsl-support-matrix.json`, sorted by filename. The
generated release note remains editable for curation and is not checksummed.
The same file verifies on every platform: `sha256sum -c SHA256SUMS` on unix,
and PowerShell `Get-FileHash -Algorithm SHA256` on Windows (see the
[Windows quickstart](quickstart-windows.md#2-verify-the-checksum)). This is the
**primary integrity mechanism** for the initially-unsigned Windows artifacts.

## Signing posture

**Initial posture: documented-unsigned, checksum-verified.** Windows artifacts
ship **without an Authenticode signature** at first. The integrity guarantee is
the SHA-256 in `SHA256SUMS`, which users verify before running.

- **SmartScreen expectation.** Running an unsigned executable triggers a Windows
  SmartScreen warning ("Windows protected your PC") on first launch. This is
  **expected and documented**, not a defect — the
  [install guide](quickstart-windows.md#3-extract--place-on-path) tells users to
  verify the checksum first and then *More info → Run anyway*. An unsigned binary
  with a verified checksum is a deliberate, stated trade-off, not a silent
  omission.
- **Authenticode upgrade path (known gap).** Removing the SmartScreen warning
  requires signing `goobers.exe` with an Authenticode certificate — ideally an
  **EV (Extended Validation) code-signing certificate**, which earns SmartScreen
  reputation immediately. That is an **organizational purchase and secret-custody
  decision** (the signing key must live in CI secrets or an HSM), so it is out of
  scope here and recorded as a known gap. When adopted, the upgrade is: obtain the
  cert, add a `signtool sign /fd SHA256 /tr <timestamp-url> /td SHA256` step to
  the [#432](https://github.com/Agent-Clubhouse/Goobers/issues/432) release
  workflow after the packaging engine emits `goobers.exe`, and update this section
  + the install guide to drop the SmartScreen note.

macOS notarization is the analogous gap on that platform; it is tracked with the
same "documented-unsigned first" posture wherever the macOS release story is
written.

## Distribution channels (scoop / winget)

The Homebrew-tap analog on Windows is **scoop** and **winget**. Per the
cross-platform design ([P12](../design/cross-platform-support.md)), these are
**documentation-level only** for now: the manifest *shape* and package *identity*
are recorded here so the names are reserved-by-design, but no published manifest
is maintained until the Windows node story
([#647](https://github.com/Agent-Clubhouse/Goobers/issues/647) /
[#752](https://github.com/Agent-Clubhouse/Goobers/issues/752)) justifies the
upkeep. **Installing from the release zip
([Windows quickstart](quickstart-windows.md)) is the supported path first.**

### scoop app manifest shape

A scoop manifest is a JSON file (`goobers.json`) that would live in a scoop
bucket. The intended shape, driven by the artifact names above:

```json
{
  "version": "1.2.3",
  "description": "Goobers agent-workforce daemon and CLI.",
  "homepage": "https://github.com/Agent-Clubhouse/Goobers",
  "license": "See repository",
  "architecture": {
    "64bit": {
      "url": "https://github.com/Agent-Clubhouse/Goobers/releases/download/v1.2.3/goobers_v1.2.3_windows_amd64.zip",
      "hash": "<sha256 from SHA256SUMS>"
    }
  },
  "bin": "goobers.exe",
  "checkver": "github",
  "autoupdate": {
    "architecture": {
      "64bit": {
        "url": "https://github.com/Agent-Clubhouse/Goobers/releases/download/v$version/goobers_v$version_windows_amd64.zip"
      }
    }
  }
}
```

The `hash` maps directly to the artifact's line in `SHA256SUMS`; `autoupdate`
tracks GitHub releases. Only `64bit` (amd64) is defined — consistent with
[the arm64 decision](#windowsarm64-deferred). **Publication trigger:** stand up a
scoop bucket (repo or a `scoop-goobers` repo) and populate this manifest once
[#432](https://github.com/Agent-Clubhouse/Goobers/issues/432) publishes tagged
releases *and* a Windows node is a supported target
([#647](https://github.com/Agent-Clubhouse/Goobers/issues/647) verdict).

### winget package identity

winget packages are keyed by a `PackageIdentifier` in `Publisher.Package` form,
submitted to `microsoft/winget-pkgs`. The reserved-by-design identity:

| Field | Value |
|---|---|
| `PackageIdentifier` | `AgentClubhouse.Goobers` |
| `PackageName` | `Goobers` |
| `Publisher` | `Agent Clubhouse` |
| `Moniker` | `goobers` |
| `InstallerType` | `zip` (with a nested-`goobers.exe` `RelativeFilePath`) |
| `Architecture` | `x64` only |

**Publication trigger:** submit the winget manifest to `microsoft/winget-pkgs`
once releases are tagged+published (#432) and the Windows target is supported
(#647) — same gate as scoop. Recording the identity now reserves
`AgentClubhouse.Goobers` / the `goobers` moniker so a later submission is
uncontested.

## windows/arm64 (deferred)

`windows/arm64` is **not a published artifact.** Go cross-compiles it cheaply,
but nothing in CI or on a real machine has executed a Windows/arm64 build, and
shipping a never-run binary is exactly the false-green trap the release gate
exists to prevent. It is therefore **excluded from `DefaultTargets`** in the
packaging engine (enforced by a test) and from the scoop/winget shapes above.

**Promotion trigger:** add `windows/arm64` to `DefaultTargets` and the
distribution manifests once a Windows/arm64 build has actually been run — either
a live arm64 Windows machine or a CI leg that executes (not just compiles) the
arm64 binary. Until then the decision is *deferred, with evidence required to
ship*.

## The Windows gate

Publishing a Windows binary is gated on the Windows CI leg
([#633](https://github.com/Agent-Clubhouse/Goobers/issues/633)) being green.
This is not ceremony: today `GOOS=windows go build ./cmd/goobers` **fails to
compile** — `internal/platform/proc` has no Windows implementation yet (the Job
Objects rung of the [#620–#627](https://github.com/Agent-Clubhouse/Goobers/issues/623)
process-control abstraction chain). Releasing binaries for a platform CI does not
even compile would recreate the false-green trap at distribution scale.

The packaging engine reflects this: by default it **fails** if a requested target
does not compile (surfacing the real build error), so a release can never
silently drop or ship-broken the Windows target. `-skip-unbuildable` packages
only what compiles (for producing the unix artifacts while Windows is pending),
and prints exactly which targets were skipped. When `internal/platform/proc`'s
Windows implementation lands and #633 is green, `windows/amd64` builds and
packages with no further change to the engine.
