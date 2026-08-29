# Large-repo mode

Enable the per-repository preset in `instance.yaml`:

```yaml
repos:
  - provider: github
    owner: acme
    name: monolith
    token:
      env: GOOBERS_GITHUB_TOKEN
    largeRepo: true
```

The declaration resolves to:

| Setting | Default |
|---|---|
| Workspace | Pinned at `workcopies/<repo-key>/pin` |
| Concurrency | One whole-run lease; runs targeting the repo are serial |
| Pinned clean policy | `none` |
| Deterministic stage timeout | `4h` |
| Stalled-run watchdog | `6h` |
| Maximum run duration | `24h` |
| Path-length preflight | Enabled, 260-character ceiling |
| Mirror refresh | Heads and tags only |
| Deterministic stage environment | `MSBUILDDISABLENODEREUSE=1` |
| Worktree retention | Exempt by construction because the pinned workspace is outside `runs/` |

`goobers validate` prints the resolved values for each enabled repository.
`goobers config show` renders those effective values as YAML or JSON.

The preset supplies defaults rather than locking the settings. Explicit
repository settings win. For example, this keeps large-repo mode enabled while
tightening warm-build timeouts and relaxing the workspace and path policies:

```yaml
repos:
  - provider: github
    owner: acme
    name: monolith
    token:
      env: GOOBERS_GITHUB_TOKEN
    largeRepo: true
    workspace:
      pinned: false
    pathLength:
      disabled: true
    defaultStageTimeout: 45m
    runControls:
      stalledRunTimeout: 90m
      maxRunDuration: 8h
```

`workspace.pinned: false` disables the pinned workspace, whole-run lease,
retention exemption, and narrowed pinned-mirror refresh together because those
properties are structural consequences of pinned execution. Per-stage
`timeoutSeconds`, gaggle `runControls`, and workflow `runControls` remain more
specific than the repository defaults. A deterministic stage can override the
MSBuild setting through its own `run.env`.

Windows operators should also follow the
[Windows large-repo runbook](windows-large-repo-runbook.md) for Defender/Dev
Drive setup, build-process cleanup, and the exact environment-isolation
boundary.
