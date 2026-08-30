# Windows large-repo runbook

Use this runbook with [large-repo mode](large-repo-mode.md) for legacy .NET
Framework, C++, and other large repositories hosted on Windows.

## Workcopies storage and antivirus

Microsoft Defender can dominate checkout and build time when it scans every
small file written beneath the workcopies root. Prefer one of these host-level
mitigations:

1. Put the instance workcopies root on a
   [Windows Dev Drive](https://learn.microsoft.com/windows/dev-drive/).
2. Add the workcopies root to Microsoft Defender's path exclusions, following
   your organization's security policy. Review the exclusion with the security
   owner before applying it.

Goobers checks this posture when `goobers up` or a standalone `goobers run`
starts with a `largeRepo: true` repository. It warns when the root is neither
on a Dev Drive nor excluded, or when Windows does not allow the check. It never
changes Defender settings or creates a Dev Drive.

### Every directory Goobers writes then immediately reads

The workcopies root is the largest of several directories where real-time
scanning can hold a handle on a just-written file and lose the race against the
read that follows — run journals, the scheduler ledger, the blob store, the
worker's `--work-root`, a Windows stage pod's `C:\workspace` and `TEMP`. The
symptom is an unrelated-looking git error minutes later
(`unable to access '.../repo.git/config': Permission denied`, #3161–#3164).

`goobers doctor --av-exclusions [--work-root <dir>] [instance-root]` lists the
full set, derived from the same path code the daemon, worker and stage pod use,
and on a Windows host reports each directory as excluded, not-excluded, or
unknown against Microsoft Defender's exclusion list (`--report json` for
tooling). It is advisory: exit 0 whatever the coverage, and it never changes
Defender settings. `goobers up`, `goobers worker` and every Windows stage pod
print the same verdict once at startup (`av-exclusions (advisory, ...)`), so a
pod's own log answers the question after the fact.

**Per-gaggle workcopies roots are enumerated separately.** A gaggle may set
`spec.workcopies.root` to any absolute path, and that override beats the
instance-wide `workcopies.root`, so a gaggle relocated to another drive has a
mirror-and-worktree directory that is under no other listed path. The doctor
reads the gaggle inventory from `config/` to name each one, and the daemon's
startup advisory uses the same inventory it provisions from. If the config
directory cannot be read, both say so explicitly rather than reporting coverage
over a set that is silently short — treat
`per-gaggle workcopies roots are NOT enumerated` as an incomplete answer, not a
clean one.

Two matching caveats when you feed the list to your own tooling. Coverage
matching approximates Defender's documented wildcard rules and errs toward
under-reporting: `*` and `?` stand for characters within one path component and
never span `\`, and an entry written `C:\workspace\*` does not count as covering
`C:\workspace` itself. So a spurious `not-excluded` is possible; a spurious
`excluded` should not be.

Declare the answer per runner in `instance.yaml` — each `runners:` entry with
`provides.os: windows` should carry `provides.windows.avExclusionsVerified:
true` (or `false`, honestly); `goobers validate` warns `RNR006` otherwise. The
claim is trusted like every other `provides:` claim, never re-verified.
`RNR006` is **strict-neutral**: `goobers validate --strict` prints it but does
not promote it to an error, so adding a Windows runner — or upgrading into this
version with one already declared — cannot turn a green CI pipeline red. That
carve-out is deliberate: declaring `false` does not silence the warning, so
promoting it would leave `true` as the only way to get green and put an operator
under CI pressure to assert a claim they had not earned.

## Build processes and file locks

Large-repo mode defaults deterministic stages to
`MSBUILDDISABLENODEREUSE=1`. This prevents reusable MSBuild workers from
retaining loaded-assembly locks after a build. A stage can opt back into node
reuse after process cleanup is proven reliable:

```yaml
run:
  command: [build.cmd]
  env:
    MSBUILDDISABLENODEREUSE: "0"
```

The process-tree cleanup seam must account for orphaned `MSBuild.exe` workers
and `VBCSCompiler.exe`. Before resetting or manually deleting a pinned
workspace, stop build daemons that still hold files beneath it.

## Environment isolation contract

Each stage has an isolated working directory, environment-variable set, and
process tree. Environment changes made by one deterministic stage do not carry
into the next stage because each stage is a separate subprocess.

Bootstrap a required toolchain in the stage's own command rather than mutating
the daemon environment. For example:

```yaml
run:
  command:
    - cmd.exe
    - /D
    - /S
    - /C
    - '"C:\BuildTools\VC\Auxiliary\Build\vcvarsall.bat" x64 && build.cmd'
```

Goobers does **not** isolate machine-global state such as the registry, GAC or
COM registration, global caches, or services. Large-repo mode's whole-run
lease prevents runs for the same repository from interleaving, but it does not
protect unrelated gaggles from machine-wide mutations. Repositories whose
builds mutate machine state should run on a dedicated host and should not share
a node with unrelated gaggles.
