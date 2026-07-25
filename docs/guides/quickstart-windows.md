# Windows quickstart (tier 1, local)

Stand up a `goobers` node on Windows from scratch: install prerequisites,
install or build the binary, configure credentials, drive a first run, and run
the daemon in the foreground. This is the Windows-specific companion to the
platform-neutral [`quickstart.md`](quickstart.md).

Windows is **officially supported for deterministic workloads**. The required
Windows CI gate runs the real foreground daemon and the complete shipped
implementation workflow through the local runner with a deterministic fake
harness. That validation is sufficient for the deterministic-only support
milestone. Live Copilot CLI/agentic parity remains the independent
[#647 Tier 2](https://github.com/Agent-Clubhouse/Goobers/issues/647) scope and is
not a blocker to using deterministic workflows on Windows.

## Validated environment and evidence

The `windows gate (build · vet · runtime smoke)` job in
`.github/workflows/ci.yml` runs on `windows-latest` for every change. A
representative successful hosted run on 2026-07-25 used:

| Component | Validated on |
|---|---|
| Operating system | Microsoft Windows Server 2025, `Microsoft Windows [Version 10.0.26100.32995]`, runner image `windows-2025-vs2026` version `20260714.173.1` |
| Architecture | `windows/amd64` |
| Go toolchain | Go 1.26.5 (the version pinned in [`go.mod`](../../go.mod)) |
| Git | Git for Windows 2.55.0.windows.2 |

Each run uploads a `windows-validation-evidence` artifact containing:

- `environment.txt`, including the exact Windows version/build reported by
  `cmd.exe /c ver`, runner image, Git, Go, and Goobers versions;
- `implementation-workflow.json` plus the captured run journal, proving the
  fake-harness implementation path reached `phase=completed` through
  `query-backlog`, implementation, review, local gate, PR/CI gates, and
  `close-out`;
- daemon status/logs and its scheduler journal, including
  `daemon.clean_shutdown`; and
- `summary.md`, including the breakage triage list. The current list is empty.

Reproduce the source-level validation from a repository checkout:

```powershell
go build -o bin\goobers.exe .\cmd\goobers
go run .\test\windowsvalidate -bin bin\goobers.exe -out bin\windows-validation-evidence
Get-Content .\bin\windows-validation-evidence\summary.md
```

The validation is credential-free and does not invoke an external agent CLI.
Its fake replaces external effects while the real runner, worktrees, journal,
stage envelopes, and gates execute normally.

## 1. Install prerequisites

Use a 64-bit Windows 11 or Windows Server 2025 host with:

1. **Git for Windows 2.40 or newer** on `PATH`. Goobers relies on modern
   worktree and per-command Git configuration behavior.
2. **PowerShell** for the commands below.
3. **Go matching `go.mod`** only when building from source or reproducing the
   validation. A release install does not require Go, Node.js, Bash, or Make.
4. Every executable used by your own deterministic workflow stages on the
   daemon's `PATH`.

Enable Windows long-path support from an elevated PowerShell, enable Git's
matching support, and prefer a short instance root such as `C:\goobers`:

```powershell
New-ItemProperty `
  -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem' `
  -Name LongPathsEnabled -Value 1 -PropertyType DWord -Force
git config --global core.longpaths true
```

See [Windows worktree notes](windows-worktree-notes.md) for symlink behavior,
CRLF policy, and the full path-length calculation.

## 2. Verify the checksum

Download `goobers_<version>_windows_amd64.zip` and `SHA256SUMS` from the same
tagged release. Only `windows/amd64` is published; Windows ARM64 is deferred.
Release artifacts are initially unsigned, so checksum verification is required:

```powershell
$archive = Get-ChildItem .\goobers_*_windows_amd64.zip
$want = (Select-String -Path .\SHA256SUMS -Pattern ([regex]::Escape($archive.Name))).Line.Split(' ')[0]
$got = (Get-FileHash -Algorithm SHA256 $archive.FullName).Hash.ToLower()
if ($got -ne $want) { throw "CHECKSUM MISMATCH: $got != $want" }
"OK: checksum matches"
```

## 3. Extract & place on PATH

A per-user install needs no elevation:

```powershell
$dest = "$env:LOCALAPPDATA\Programs\goobers"
New-Item -ItemType Directory -Force -Path $dest | Out-Null
Expand-Archive -Path .\goobers_*_windows_amd64.zip -DestinationPath $dest -Force

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath -notlike "*$dest*") {
  [Environment]::SetEnvironmentVariable('Path', "$userPath;$dest", 'User')
}
```

Open a new terminal, then confirm the installed binary:

```powershell
goobers --version
```

For a machine-wide install, extract to `C:\Program Files\goobers` from an
elevated prompt and update the machine `PATH`. Because the binary is unsigned,
SmartScreen may warn on first launch. After verifying the checksum, choose
**More info → Run anyway**. See [Releases & packaging](releases.md) for the
artifact and signing posture.

## 4. Build from source instead

From a repository checkout with the pinned Go toolchain:

```powershell
go build -o bin\goobers.exe .\cmd\goobers
.\bin\goobers.exe --version
```

The committed portal assets are embedded, so Node/npm is not needed for the CLI
build. Use `go run .\test\ci`, not `make ci`, for the repository's portable
development gate on a shell-less Windows host.

## 5. Scaffold and configure an instance

```powershell
goobers init C:\goobers\my-instance
```

Edit `instance.yaml` to point at your repository. Never inline a provider
secret. For an interactive first run, reference an environment variable in the
config and set it before starting Goobers:

```powershell
$env:GOOBERS_GITHUB_TOKEN = '<token from your secret manager>'
```

For a file-backed secret, remove inherited ACLs and grant only the current user
full access. Goobers checks the Windows DACL and fails closed on a broadly
readable file; Unix `chmod 600` is not meaningful on NTFS.

```powershell
$tokenDir = "$env:USERPROFILE\.config\goobers"
$tokenFile = "$tokenDir\github.token"
New-Item -ItemType Directory -Force -Path $tokenDir | Out-Null
Set-Content -NoNewline -Path $tokenFile -Value $env:GOOBERS_GITHUB_TOKEN
icacls.exe $tokenFile /inheritance:r /grant:r "$($env:USERNAME):F"
```

Set the repository token reference to `env: GOOBERS_GITHUB_TOKEN` or
`file: C:\Users\<you>\.config\goobers\github.token`, then validate:

```powershell
goobers validate C:\goobers\my-instance
```

## 6. Drive a first run

Run one configured workflow manually:

```powershell
goobers run <workflow-name> C:\goobers\my-instance
goobers status C:\goobers\my-instance
goobers trace <run-id> C:\goobers\my-instance
```

The credential-free fake-harness path is the source-level validation command in
[Validated environment and evidence](#validated-environment-and-evidence);
`goobers init --demo` remains limited to hosts with native network isolation.

## 7. Run the daemon in the foreground

```powershell
goobers up C:\goobers\my-instance
```

`goobers up` runs the scheduler and local runner in the foreground. From a
second terminal, confirm it is healthy:

```powershell
goobers status --daemon C:\goobers\my-instance
```

Press Ctrl+C or Ctrl+Break in the daemon console to request a graceful drain.
The Windows validation uses Ctrl+Break against a dedicated console and
requires a clean exit plus a `daemon.clean_shutdown` journal event.

This foreground/manual lifecycle is the scope validated here. For unattended
operation under the Service Control Manager, follow
[Daemon supervision → Windows](supervision.md#windows-windows-service); Windows
Service installation is a separate deployment concern.

## Deltas from the macOS/Linux flow

| Aspect | Windows behavior |
|---|---|
| Distribution | `.zip` + PowerShell `Get-FileHash`; only `windows/amd64` |
| Build/CI command | `go build` / `go run .\test\ci`; Bash and Make are not required by the platform |
| Paths/worktrees | Enable long paths, use a short instance root, and define repository line endings in `.gitattributes` |
| Token files | Owner-only NTFS DACL checked with Windows APIs; do not use `chmod` |
| Foreground shutdown | Ctrl+C/Ctrl+Break; Windows has no SIGTERM |
| `network: none` | Fails closed because Windows has no native isolation backend; trusted-local operators may explicitly set `GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE=1` |
| Agentic stages | Live Copilot CLI support remains #647 Tier 2; deterministic-only support does not depend on it |
| Service supervision | SCM setup is documented separately and is not part of the foreground validation |

All other CLI and journal semantics are shared across platforms.
