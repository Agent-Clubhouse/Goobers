# Windows quickstart (tier 1, local)

**Start here on Windows.** Complete sections 1 through 3 to install
prerequisites, verify the release, and put `goobers` on `PATH` (or use section
4 to build from source). Then choose one route:

- return to the platform-neutral [quickstart tutorial](quickstart.md) for
  disposable learning; or
- continue with section 5 and
  [Onboard an arbitrary repository](arbitrary-repo-onboarding.md) to configure
  a real instance, using `goobers init --guided` if you
  want the browser wizard.

The remaining sections contain Windows-specific credential, WSL isolation,
path-length, and service guidance. They supplement rather than duplicate either
route.

Windows is **officially supported for deterministic workloads**. The required
Windows CI gate runs the real foreground daemon and the complete shipped
implementation workflow through the local runner with a deterministic fake
harness. That validation is sufficient for the deterministic-only support
milestone. Live Copilot CLI/agentic parity remains the independent
[#647 Tier 2](https://github.com/Agent-Clubhouse/Goobers/issues/647) scope and is
not a blocker to using deterministic workflows on Windows.

Workflows that require the full Linux isolation posture can run through WSL 2
instead of weakening `network: none` or agentic-stage confinement. Goobers
provides an explicit readiness check and handoff for that route; see
[Full isolation through WSL 2](#full-isolation-through-wsl-2).

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
npm --prefix portal ci --no-audit --no-fund
npm --prefix portal run build
go build -tags embed_portal -o bin\goobers.exe .\cmd\goobers
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
`goobers.exe` is Authenticode-signed, but verify the checksum too — it's
recomputed after signing, so it always covers the exact published bytes:

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
elevated prompt and update the machine `PATH`. See
[Releases & packaging](releases.md) for the artifact and signing posture.

## 4. Build from source instead

From a repository checkout with the pinned Go and Node.js 24 toolchains:

```powershell
npm --prefix portal ci --no-audit --no-fund
npm --prefix portal run build
go build -tags embed_portal -o bin\goobers.exe .\cmd\goobers
.\bin\goobers.exe --version
```

The Portal build produces the ignored reusable asset artifact embedded by the
tagged Go build. Use `go run .\test\ci`, not `make ci`, for the repository's
portable development gate on a shell-less Windows host.

## 5. Windows instance and credential deltas

Keep the instance root outside the target repository and use a short path for
worktree path-length headroom. Choose whether the config source is
instance-local, an in-repo subtree, or a separate config repository using the
[instance and config placement guide](instance-placement.md).

Follow the initialization and configuration steps in
[Onboard an arbitrary repository](arbitrary-repo-onboarding.md),
using a short root such as `C:\goobers\my-instance`. Never inline a provider
secret. For an interactive run, reference an environment variable in the
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

GNU Make is not installed by default on Windows, and Goobers does not require
or auto-install it (bring-your-own runner). A gaggle's `ciCommand`
(`GaggleSpec.CICommand`) overrides the `local-ci` stage's default `["make",
"ci"]`, so point it at a make-free command your project actually has.

`ciCommand` is a field on the Gaggle resource, not a standalone file — edit
the `spec:` block of the gaggle generated by Getting Started, at
`config\gaggles\<your-gaggle>\gaggle.yaml` under your instance root (e.g.
`C:\goobers\my-instance\config\gaggles\my-app\gaggle.yaml`). Add `ciCommand`
as a sibling of the gaggle's other `spec` fields (`project`, `backlog`,
`isolation`, ...), indented two spaces under `spec:` like the rest of them —
e.g. for a Go project:

```yaml
spec:
  # ...project, backlog, isolation, and any other fields already here stay as generated...
  ciCommand: ["go", "test", "./..."]
```

Then re-validate:

```powershell
goobers validate C:\goobers\my-instance
```

Before any run executes, Goobers resolves `ciCommand`'s first token through
`PATH` and fails the run immediately (as `ci-command-preflight`) if it can't
be found, naming the missing executable — rather than retrying a command that
was never going to work.

The credential-free demo in the canonical quickstart requires native network
isolation and is therefore unavailable to native Windows. Use the
credential-free fake-harness command in
[Validated environment and evidence](#validated-environment-and-evidence), or
follow the canonical demo through WSL 2.

## Full isolation through WSL 2

Use this route for agentic workflows or any workload that must retain enforced
network isolation. It is an alternative execution substrate, not a relaxation
of the native-Windows policy.

Goobers considers a WSL environment ready only when all of these checks pass:

1. `wsl.exe` can start the selected (or default) installed distro.
2. The distro is running under **WSL 2**. WSL 1 is rejected because it does not
   provide the Linux namespace behavior this route requires.
3. A Linux build of `goobers` and `bwrap` (Bubblewrap) are on the distro's
   `PATH`.
4. The Linux Goobers binary can execute the same unprivileged user + network
   namespace path used by `network: none`, and Bubblewrap can start the
   agentic-stage filesystem/PID sandbox. These are real execution probes rather
   than inferences from configuration files.

Install WSL and a distro from an elevated PowerShell if needed:

```powershell
wsl.exe --install -d Ubuntu-24.04
```

Inside that distro, install Bubblewrap plus the Linux Goobers build and the
workflow's Linux-side dependencies (Git, Copilot CLI, language toolchains).
For Ubuntu/Debian:

```bash
sudo apt-get update
sudo apt-get install --yes bubblewrap git
# Install the goobers Linux release binary on PATH, then:
goobers --version
```

Back in PowerShell, check the default distro or select one explicitly:

```powershell
goobers preflight
goobers preflight --distro Ubuntu-24.04
```

The check fails closed and prints remediation for a missing distro, WSL 1,
missing Linux binary/Bubblewrap, or blocked namespaces. When it reports ready,
hand off a command by placing its normal Goobers arguments after `--`:

```powershell
Set-Location C:\goobers
goobers preflight --distro Ubuntu-24.04 --launch-wsl -- init my-instance
goobers preflight --distro Ubuntu-24.04 --launch-wsl -- run implementation my-instance
goobers preflight --distro Ubuntu-24.04 --launch-wsl -- up my-instance
```

The handoff maps the current Windows directory as the WSL working directory,
loads the distro user's login environment, and then replaces that shell with
the discovered Linux `goobers` binary. Forwarded arguments are positional (not
evaluated as shell text), the terminal remains attached, and the child
command's exit code is preserved. Use paths relative to that working directory,
as above, or Linux paths. Build and configure the instance through the WSL
route; Windows absolute paths stored inside configuration are not Linux paths.
Credentials and tool installations must also be available to the distro user
rather than relying on the native Windows process environment.

## 6. Run the daemon in the foreground

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
