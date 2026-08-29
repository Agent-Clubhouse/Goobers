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
`install.sh`, `goobers-agent-toolkit_<version>.zip`,
`goobers-onboarding_<version>.zip`, `feature-registry.json`,
`dsl-support-matrix.json`, and `RELEASE_NOTES.md`. The release body and attached
notes are the same document:
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
$EDITOR .github/release-notes/vMAJOR.MINOR.PATCH.md
git add .github/release-notes/vMAJOR.MINOR.PATCH.md
git commit -m "docs: curate vMAJOR.MINOR.PATCH release notes"
git tag vMAJOR.MINOR.PATCH
git push origin vMAJOR.MINOR.PATCH
```

### Pre-release tags

A tag may also carry a SemVer 2.0.0 pre-release suffix —
`vMAJOR.MINOR.PATCH-<identifier>[.<identifier>...]`, e.g. `v1.3.0-beta.2` or
`v1.3.0-rc.1`. Pushing one runs the **identical** full pipeline as a stable
tag: the same packaging matrix, the same macOS notarization and Windows
Authenticode signing, and the same curated-release-note requirement at
`.github/release-notes/<tag>.md` (the literal tag string, dash and all, e.g.
`v1.3.0-beta.2.md`).

The only behavioral difference is at publication: the workflow detects the
`-` in the tag and passes `--prerelease` to `gh release create`/`gh release
edit`, so GitHub flags it as a pre-release and it never becomes the repo's
"Latest release". The feature-registry and DSL-support-matrix baseline a
pre-release ships still diffs against the **last stable** release (never
another pre-release), so `goobers features`/support-matrix deltas stay
anchored to real release lines.

This is unrelated to the DSL feature/support-matrix registry's own
`vMAJOR.MINOR.PATCH` version requirement (`internal/supportmatrix`,
`internal/workflow/v_3_0`, `internal/workflow/v_next`) — that tracks which
*stable* release a DSL feature or version shipped or was deprecated in, and
intentionally does not accept pre-release identifiers. A pre-release tag
never appears in that lineage.

## Install a pinned release

On Linux or macOS, run the installer attached to the current `v0.1.0` release:

```sh
VERSION=v0.1.0
/bin/sh -c "$(curl -fsSL "https://github.com/Agent-Clubhouse/Goobers/releases/download/${VERSION}/install.sh")" \
  -- "$VERSION"
```

The command downloads only assets attached to that tag. The helper detects the
host OS and architecture, downloads the matching archive plus `SHA256SUMS`,
verifies the archive before extraction, and installs `goobers` to
`$HOME/.local/bin` (override with `GOOBERS_INSTALL_DIR`). It retains the exact
binary as `goobers-<version>` and refreshes `goobers` as the current-version
convenience command. It installs the
archive's `README.md`, `docs/` tree, and `onboarding/` payload to the versioned
`${XDG_DATA_HOME:-$HOME/.local/share}/goobers/<version>` directory (override the
root with `GOOBERS_DOCS_DIR`), so installing a newer release does not replace
earlier documentation, templates, or sample. Installation ends there: the
default run configures nothing and prints the next steps — the credential-free
demo and guided setup — so the install result never depends on setup choices.
To chain guided setup in the same run, opt in with
`--guided [instance-path]` (default `./goobers-instance`): the installer then
runs the release binary's `goobers init --guided` flow, which separately
selects a checked-in config source and target application repository, prompts
for credential references and canonical workflows, and validates both source
and instance. Use a fresh instance path; a new source path must also be empty,
while an adopted source is validated and left unchanged. A failed or canceled
guided setup is reported separately from the successful install and sets the
script's exit status.

The helper intentionally delegates all config generation and validation to the
installed binary. The release-pinned README and platform-neutral quickstart
invoke `goobers-<version>`, not the replaceable current-version command. The
canonical workflow templates are embedded in that tagged binary and mirrored
byte-for-byte under the installed `onboarding/` path, so the same tag selects the
same checksummed archive, starter config, and tutorial target without reading
the development repository's moving `main`.
`curl`, `tar`, and either `sha256sum` (Linux) or `shasum` (macOS) are required.
Windows adopters should use the checksum-verified
[Windows release path](quickstart-windows.md).

## The packaging engine

`go run ./release` cross-compiles `./cmd/goobers` for the release matrix,
regenerates the CLI reference, man pages, and completion scripts from that
release's command registry, packages each target with the tagged checkout's
documentation into a platform-conventional archive, and writes a shared
`SHA256SUMS` manifest, the tagged install helper, portable agent toolkit,
standalone onboarding payload, generated release notes, and shipped DSL feature
and version-support snapshots into `dist/` (override with `-output`). It is a
standalone Go tool — matching `test/ci` and `test/coveragegate` — so it runs
identically on any release runner without a shell dependency.

```sh
go run ./release -first-feature-snapshot      # first recorded snapshot only
go run ./release -previous-features previous/feature-registry.json \
  -previous-support-matrix previous/dsl-support-matrix.json
go run ./release -previous-features previous/feature-registry.json \
  -previous-support-matrix previous/dsl-support-matrix.json -targets windows/amd64
go run ./release -previous-features previous/feature-registry.json \
  -previous-support-matrix previous/dsl-support-matrix.json -version vMAJOR.MINOR.PATCH -output dist
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
The same staging pass adapts the bundled README and quickstarts to the installed
archive: the README's installer step and the quickstarts' build steps become a
tagged binary check, and walkthrough commands invoke `goobers` from `PATH`.
The tagged workflow then regenerates those three directories with the extracted
binary and diffs them against the archive, so a release cannot publish CLI docs
from another version. It also runs the packaged quickstart's initial `init` and
`validate` commands with the extracted binary.

The starter configuration and scaffold templates remain compiled into that same
binary and are emitted by `goobers init`/`goobers scaffold`. The onboarding
payload is a byte-identical, inspectable mirror, not a second mutable template
source. Keeping the extracted archive together therefore gives an installation
one release identity across the binary, onboarding docs, shell completions, man
pages, generated templates, and tutorial target. Start with the bundled
`README.md`, use `docs/VISION.md` and `docs/ARCHITECTURE.md` for the product
concepts, then follow `docs/guides/quickstart.md`.

### Release-pinned workflow templates and sample

Every platform archive contains `onboarding/`, and every GitHub Release also
publishes the same directory as
`goobers-onboarding_<version>.zip`. `SHA256SUMS` covers both forms. The
standalone asset is useful for consumers that need only tutorial inputs; the
installer uses the copy already inside the verified platform archive.

```text
onboarding/
  manifest.json
  templates/
    canonical/                         # tagged config-examples, excluding Go embed code
    quickstart@v1/                     # ONB-A2 happy-path template
  samples/
    getting-started-task-api@1.0.0/    # ONB-A3 app, sample.json, and seed-issues.json
```

`manifest.json` records the producing release version and commit, the canonical
template's release version, the quickstart template version, the sample version
and compatible templates, and every payload file's size, mode, and SHA-256.
Packaging copies these files directly from the tagged checkout; release tests
compare every copy with its source, and release verification compares the
standalone zip with the copy in the platform archive.

The immutable hosted path is
`https://github.com/Agent-Clubhouse/Goobers/releases/download/<version>/goobers-onboarding_<version>.zip`.
After installation, the same bytes resolve under
`${GOOBERS_DOCS_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/goobers}/<version>/onboarding`.
The release-pinned quickstart substitutes the concrete installed version into
that path when it materializes the sample.
`goobers-<version> init --template=quickstart` resolves its template from the
retained versioned binary; its emitted config must match
`onboarding/templates/quickstart@v1` byte-for-byte. Guided setup uses the
canonical templates compiled into that same binary, mirrored under
`onboarding/templates/canonical`. Installing a later release creates a sibling
version directory and binary and cannot change either earlier resolution path.

### Portable agent toolkit

Every non-empty release build writes
`goobers-agent-toolkit_<version>.zip`. Its `manifest.json` identifies the
producing release and commit, compiled DSL support matrix, compatible harness
adapters, CLI command requirements, and every product-owned payload asset with
its SHA-256 digest. The copy-ready product boundary is
`payload/.goobers/agent-toolkit/`; user-owned root instructions are outside it.

The payload carries canonical Agent Skills plus docs, schemas, examples, and
the capability registry copied from the same source revision as the binary.
Release tests validate the manifest schema and golden layout, recalculate every
asset digest, compare representative references with their release sources,
resolve every adapter skill, and reject secret-shaped content and
machine-specific paths.

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
| `windows/amd64` | `goobers_<version>_windows_amd64.zip` | `goobers.exe`, `README.md`, `docs/`, `onboarding/` |
| `darwin/amd64`, `darwin/arm64` | `goobers_<version>_<os>_<arch>.tar.gz` | `goobers`, `README.md`, `docs/`, `onboarding/` |
| `linux/amd64`, `linux/arm64` | `goobers_<version>_<os>_<arch>.tar.gz` | `goobers`, `README.md`, `docs/`, `onboarding/` |
| platform-independent | `goobers-onboarding_<version>.zip` | `manifest.json`, `templates/`, `samples/` |

Windows uses `.zip` (the platform convention Windows users and scoop/winget
expect); unix targets use `.tar.gz`.

### Checksums

`SHA256SUMS` is a coreutils `sha256sum -c`-compatible manifest — one
`<hex>  <filename>` line per binary archive, `install.sh`, portable agent
toolkit, onboarding payload, and authoritative `feature-registry.json` and
`dsl-support-matrix.json`, sorted by filename. The generated release note
remains editable for curation and is not checksummed. The same file verifies on
every platform: `sha256sum -c SHA256SUMS` on unix, and PowerShell
`Get-FileHash -Algorithm SHA256` on Windows (see the
[Windows quickstart](quickstart-windows.md#2-verify-the-checksum)). This
integrity check is in addition to, not instead of, the Authenticode
signature below — both `sign-macos` and `sign-windows` recompute this
manifest after signing, so it always reflects the signed bytes actually
published.

## Signing posture

- **macOS: Developer ID signed and notarized.** The `sign-macos` job in
  [`release.yml`](../../.github/workflows/release.yml) imports a Developer ID
  Application certificate, signs each darwin binary
  (`codesign --options runtime --timestamp`), and submits it to Apple's notary
  service (`xcrun notarytool submit --wait`) before the archive is published.
  This is **online-only** notarization: `stapler staple` only applies to
  `.app`/`.pkg`/`.dmg` bundles, not a bare Mach-O executable, so no
  notarization ticket is stapled to `goobers` itself. Gatekeeper's online
  ticket check is gated on the `com.apple.quarantine` extended attribute,
  which browser downloads set and the documented curl-pipe `install.sh` path
  does not — so the documented install path never triggers a Gatekeeper
  block either way, and a user who manually downloads the tar.gz from the
  Releases page gets the full online-verified benefit of signing. A stapled
  `.pkg`/`.dmg` installer (for fully offline Gatekeeper coverage) is a
  distinct-scope follow-up, not built here.

- **Windows: Authenticode signed via Azure Trusted Signing.** The
  `sign-windows` job in
  [`release.yml`](../../.github/workflows/release.yml) authenticates to
  Azure via OIDC (`azure/login`, no stored client secret) and signs
  `goobers.exe` (`azure/trusted-signing-action`, SHA-256 file digest and
  RFC 3161 timestamp) before the archive is published. This reuses the
  sibling Clubhouse product's certificate profile
  (`clubhouse-win-codesign`) rather than a Goobers-specific one — a
  code-signing certificate identifies the *publisher* (Mason Allen), not a
  specific product, matching how macOS signing above already reuses one
  Apple Developer ID for both. No separate Windows identity or additional
  Azure resource is needed; the account's Basic SKU only supports one
  certificate profile, and there's no technical reason a single validated
  identity can't sign multiple products.
  `azure/trusted-signing-action` requires a Windows runner (it invokes the
  Windows SDK signing client locally; only the private-key operation
  happens in Azure).

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
  "version": "0.1.0",
  "description": "Goobers agent-workforce daemon and CLI.",
  "homepage": "https://github.com/Agent-Clubhouse/Goobers",
  "license": "See repository",
  "architecture": {
    "64bit": {
      "url": "https://github.com/Agent-Clubhouse/Goobers/releases/download/v0.1.0/goobers_v0.1.0_windows_amd64.zip",
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

## linux/arm64 and darwin/amd64 (shipped, never executed)

Unlike `windows/arm64` above, `linux/arm64` and `darwin/amd64` **are** published
`DefaultTargets` — but no CI leg or release step ever *runs* either binary. CI
executes tests only on `linux/amd64` (ubuntu runners), `darwin/arm64`
(macos-latest, now Apple Silicon), and `windows/amd64` (windows-latest); the
release workflow's own smoke test (`.github/workflows/release.yml`, "Verify
release artifacts") extracts and exercises only the `linux_amd64` archive.
That leaves two shipped arches with zero recorded runtime evidence, ever.

**Recorded decision (2026-08-01, filed from the state-of-repo review,
[#2039](https://github.com/Agent-Clubhouse/Goobers/issues/2039)): ship without
adding execution coverage, for now.** Rationale:

- The codebase is pure Go with no `cgo`, no architecture-conditional assembly,
  and no OS/arch-specific syscalls outside the already-covered
  `internal/platform/*` seam (which is exercised per-OS, not per-arch — the
  Windows/amd64 and darwin/arm64 gates already prove the *OS* seams; nothing
  in this repo branches on *arch* the way it branches on OS).
- Adding real execution requires either GitHub-hosted arm64 Linux runners and
  an Intel macOS runner (both available, at added job-minute cost) or
  QEMU/Rosetta emulation (slower, and emulates rather than proves native
  behavior) — a real cost for a risk this analysis judges low.
- This is the honest counterpart to the `windows/arm64` decision above, not a
  silent gap: unlike that deferred-and-unpublished target, these two **are**
  shipped today: a real, if judged-low-probability, risk exists that a user
  on one of these arches runs a binary this project has never once executed.

**Promotion trigger:** add a real smoke execution (a CI leg or a release-workflow
step, per-arch) if evidence emerges that arch-specific behavior actually matters
here (an arch-dependent bug report, a new `cgo` dependency, inline assembly, or
architecture-conditional code), or if the job-minute cost becomes justified
regardless. Until then, this decision stands as reviewed and deliberate, not an
accident of the release workflow's history.

## The Windows gate

Publishing a Windows binary is gated on the Windows CI leg
([#633](https://github.com/Agent-Clubhouse/Goobers/issues/633), delivered and
closed) staying green — and it is: `GOOS=windows go build ./cmd/goobers`
compiles (`internal/platform/proc`'s Job Objects implementation landed with the
[#620–#627](https://github.com/Agent-Clubhouse/Goobers/issues/623)
process-control abstraction chain), and every change runs the required
`windows gate (build · vet · runtime smoke)` job on `windows-latest`. Windows is
a supported, validated target — see the
[Windows quickstart](quickstart-windows.md) — and `windows/amd64` is in the
default release matrix, so its `.zip` artifact is built, checksummed, and
published with every release.

The packaging engine still enforces the gate's principle: by default it
**fails** if a requested target does not compile (surfacing the real build
error), so a release can never silently drop or ship-broken a target.
`-skip-unbuildable` packages only what compiles and prints exactly which
targets were skipped. With the Windows gate green, the default path builds and
packages `windows/amd64` with no special handling.
