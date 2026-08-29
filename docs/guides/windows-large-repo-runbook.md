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
