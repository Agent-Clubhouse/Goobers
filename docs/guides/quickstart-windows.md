# Windows quickstart (install & verify)

Install the `goobers` binary on Windows from a tagged release: download the
release zip, verify its checksum, place it on `PATH`, and confirm the version.
This is the Windows-specific companion to the platform-neutral
[`quickstart.md`](quickstart.md) — the CLI surface is identical; this page covers
the Windows-specific *getting-the-binary* path (zip + `Get-FileHash` verification
+ `PATH`) and, once a supervised daemon is wanted, points at the
[Windows Service](supervision.md#windows-windows-service) setup.

> **Status: distribution is staged, Windows binaries are not published yet.**
> The release-packaging engine that produces the Windows artifact is in place
> (`go run ./release`, see [Releases & packaging](releases.md)), but a *published*
> `windows/amd64` build is gated on the Windows CI leg
> ([#633](https://github.com/Agent-Clubhouse/Goobers/issues/633)) going green —
> today `GOOS=windows go build ./cmd/goobers` still fails to compile pending the
> Windows process-control implementation (`internal/platform/proc`, the
> [#620–#627](https://github.com/Agent-Clubhouse/Goobers/issues/623) abstraction
> chain). Until then, the steps below describe the supported install path for the
> first release that ships, and the [build-from-source](#build-from-source)
> fallback is the way to run on Windows in the interim. This mirrors the
> runtime-pending posture of the [Windows Service](supervision.md#windows-windows-service)
> wiring.

## 1. Download

Grab the Windows archive and the checksum manifest from the release you want
(see [Releases & packaging](releases.md) for the artifact naming scheme):

- `goobers_<version>_windows_amd64.zip` — contains `goobers.exe`, `README.md`,
  and the release-pinned `docs/` tree
- `SHA256SUMS` — the checksum manifest covering every artifact in the release

Only `windows/amd64` is published. `windows/arm64` is **not** shipped — see
[the arm64 decision](releases.md#windowsarm64-deferred).

## 2. Verify the checksum

Never skip this — the artifacts are distributed **unsigned initially** (see
[Signing posture](releases.md#signing-posture)), so the SHA-256 is the integrity
check. PowerShell's `Get-FileHash` produces the same lowercase hex the
`SHA256SUMS` manifest uses:

```powershell
# From the folder containing the .zip and SHA256SUMS:
$want = (Select-String -Path .\SHA256SUMS -Pattern 'goobers_.*_windows_amd64\.zip').Line.Split(' ')[0]
$got  = (Get-FileHash -Algorithm SHA256 .\goobers_*_windows_amd64.zip).Hash.ToLower()
if ($got -eq $want) { "OK: checksum matches" } else { throw "CHECKSUM MISMATCH: $got != $want" }
```

## 3. Extract & place on PATH

Extract `goobers.exe` and put its folder on `PATH`. A per-user install needs no
elevation:

```powershell
# Per-user install (no admin):
$dest = "$env:LOCALAPPDATA\Programs\goobers"
New-Item -ItemType Directory -Force -Path $dest | Out-Null
Expand-Archive -Path .\goobers_*_windows_amd64.zip -DestinationPath $dest -Force

# Add to the *user* PATH (persists across sessions; open a new terminal after):
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath -notlike "*$dest*") {
  [Environment]::SetEnvironmentVariable('Path', "$userPath;$dest", 'User')
}
```

For a machine-wide install, extract to `C:\Program Files\goobers` from an
elevated prompt and set the `Machine` PATH instead — this matches the path the
[Windows Service](supervision.md#windows-windows-service) example uses
(`C:\Program Files\goobers\goobers.exe`).

> **SmartScreen note.** Because the binary is unsigned, Windows SmartScreen may
> warn on first run ("Windows protected your PC"). This is expected for an
> unsigned executable whose checksum you have already verified — choose *More
> info → Run anyway*. The [Authenticode upgrade path](releases.md#signing-posture)
> removes this warning once code signing is adopted.

## 4. Confirm

```powershell
goobers --version
```

reports the same `version (commit …, built …, go… windows/amd64)` string the
release was stamped with (the packaging engine injects build metadata via the
same `internal/version` `-ldflags` path a local `make build` uses). From here
open `docs/RELEASE.md` to confirm the installed documentation identity, then use
the bundled `docs/guides/quickstart.md`. Release packaging adapts that walkthrough
to confirm the tagged binary and invoke `goobers` from `PATH`, so no source
checkout or build step is required before configuring credentials and driving a
first run.

To run the daemon under the Service Control Manager instead of a foreground
`goobers up`, follow [Daemon supervision → Windows](supervision.md#windows-windows-service).

## Windows deltas

The CLI is identical across platforms; a few Windows-specific behaviors are
documented where they live:

- **Git/worktree behavior** (long paths, `core.symlinks=false`, CRLF policy):
  see [Windows worktree notes](windows-worktree-notes.md).
- **Deterministic `network: none` stages**: the Linux user-namespace isolation
  has no Windows analog yet; the Windows execution-isolation posture is being
  decided in [#651](https://github.com/Agent-Clubhouse/Goobers/issues/651).
  Until then, treat a Windows node as trusted-local only.
- **Agent (Copilot CLI) stages**: whether agentic stages are supportable on
  Windows is under spike in
  [#647](https://github.com/Agent-Clubhouse/Goobers/issues/647); deterministic
  stages are the safe assumption first.

## Build from source

Until published Windows binaries land (gated on
[#633](https://github.com/Agent-Clubhouse/Goobers/issues/633)), build locally on
a Windows host with the Go toolchain pinned in [`go.mod`](../../go.mod):

```powershell
go build -o goobers.exe ./cmd/goobers
```

(The committed `cmd/goobers/portal-dist` assets are embedded automatically, so no
Node/npm step is needed for the CLI build.) This is also how you cross-compile a
Windows binary from another platform once the Windows compile is green:

```sh
GOOS=windows GOARCH=amd64 go run ./release -targets windows/amd64 -first-feature-snapshot
```

`-first-feature-snapshot` selects the empty baseline for this first/local
package. For later releases, use `-previous-features` as described in
[Releases & packaging](releases.md).
