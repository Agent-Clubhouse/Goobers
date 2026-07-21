# Releases & packaging

How Goobers binaries are built for distribution, packaged, and verified — and the
Windows distribution story (artifacts, checksums, signing posture, scoop/winget
shape) added by [#655](https://github.com/Agent-Clubhouse/Goobers/issues/655).

> **Scope boundary.** This page documents the release **packaging engine** and
> the **distribution shape**. It does *not* document a tagged-release
> **workflow** — the GitHub Actions automation that fires on a tag, builds the
> matrix, uploads artifacts, and writes the changelog is
> [#432 (REL-2)](https://github.com/Agent-Clubhouse/Goobers/issues/432) and is
> not built yet. The engine below is designed to be the step that workflow
> *calls*; until #432 lands, releases are produced by running the engine
> manually. **No Windows artifact is published until the Windows CI leg
> ([#633](https://github.com/Agent-Clubhouse/Goobers/issues/633)) is green** — see
> [The Windows gate](#the-windows-gate).

## The packaging engine

`go run ./release` cross-compiles `./cmd/goobers` for the release matrix,
packages each target into a platform-conventional archive, and writes a shared
`SHA256SUMS` manifest into `dist/` (override with `-output`). It is a standalone
Go tool — matching `test/ci` and `test/coveragegate` — so it runs identically on
any release runner without a shell dependency.

```sh
go run ./release                              # full matrix into ./dist
go run ./release -targets windows/amd64       # just the Windows artifact
go run ./release -version v1.2.3 -output dist # explicit version + output dir
go run ./release -skip-unbuildable            # package what compiles, skip the rest
```

Build metadata (`version`/`commit`/`date`) is injected via the same
`internal/version` `-ldflags` path the [Makefile](../../Makefile) uses, so a
released binary's `goobers --version` is byte-for-byte consistent with a local
`make build`. Version defaults to `git describe --tags --always --dirty`; the
build date defaults to the commit's committer date, so re-running the engine on
the same commit is reproducible (`-trimpath` is always on).

### Artifact shape

| Target | Archive | Contents |
|---|---|---|
| `windows/amd64` | `goobers_<version>_windows_amd64.zip` | `goobers.exe` |
| `darwin/amd64`, `darwin/arm64` | `goobers_<version>_<os>_<arch>.tar.gz` | `goobers` |
| `linux/amd64`, `linux/arm64` | `goobers_<version>_<os>_<arch>.tar.gz` | `goobers` |

Windows uses `.zip` (the platform convention Windows users and scoop/winget
expect); unix targets use `.tar.gz`. Every archive is a single-file archive of
the binary under its natural name.

### Checksums

`SHA256SUMS` is a coreutils `sha256sum -c`-compatible manifest — one
`<hex>  <filename>` line per artifact, sorted by filename. The same file verifies
on every platform: `sha256sum -c SHA256SUMS` on unix, and PowerShell
`Get-FileHash -Algorithm SHA256` on Windows (see the
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
