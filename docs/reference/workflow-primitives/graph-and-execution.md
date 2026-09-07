# Graph and execution primitives

Workflow tasks, gates, and parallels form a directed state machine. `spec.start`
names its entry state. Task `next`, gate `branches`, parallel branch `start`,
`join`, and `onFailure` fields name edges.

## Reserved targets

| Target | Valid use | Result |
| --- | --- | --- |
| omitted task `next` | Successful terminal path | Completes the run successfully. |
| `@abort` | Task/gate/parallel failure path | Ends the run as blocked/aborted. |
| `@escalate` | Task/gate/parallel escalation path | Ends the run requiring human intervention. |
| `@join` | End of a parallel branch only | Settles that branch and continues at the parallel's `join` state after all branches settle. |

`@join` is reserved but is not a run terminal. Every other target must resolve
to a declared task, gate, or parallel.

## Workspace modes

| Mode | Valid locations | Behavior |
| --- | --- | --- |
| `repo` | task `workspace`, deterministic `run.workspace`, agentic gate `workspace` | Writable worktree on the run branch. This is the historical default when omitted on local execution. |
| `repo-readonly` | task `workspace`, agentic gate `workspace` | Detached worktree at the pinned base revision; suitable for concurrent research/read-only stages. |
| `scratch` | task `workspace`, deterministic `run.workspace`, agentic gate `workspace` | Empty disposable directory with no repository checkout. |

For a deterministic task, `run.workspace` is authoritative when both it and
task-level `workspace` are present. Prefer declaring only one. `syncBase`
requires a writable `repo` workspace.

## Retry and timeout

```yaml
retry:
  maxAttempts: 3
  backoffSeconds: 30
timeoutSeconds: 900
```

| Field | Description |
| --- | --- |
| `maxAttempts` | Total attempts including the first; `1` means no retry. |
| `backoffSeconds` | Constant non-negative delay between attempts. |
| `timeoutSeconds` | Positive wall-clock bound for one task or evaluator attempt. |

Retry blocks can appear on tasks and automated/agentic evaluator
configurations. They apply only to their containing task or evaluator.

## Data-flow primitives

### `inputs`

Static string-valued task inputs. Built-in stages define their accepted keys.

### `inputsFrom`

Maps a consumer input name to an upstream scalar output:

```yaml
inputsFrom:
  prNumber: open-pr.prNumber
```

A bare output name reads the immediately preceding task. Qualified
`<stage>.<output>` references select a named producer. Parallel joins may use
branch-qualified references. Missing declared outputs fail the consuming stage
closed.

### `contextFrom`

Restricts rich artifact/verdict pointers to named producer tasks or gates.
Empty preserves the historical behavior of receiving all accumulated context.

### `expectedOutputs`

Declares scalar outputs or artifacts that later states rely on. Shell stages
that emit scalar outputs use an `inputs.resultFile` contract; kind-backed and
agentic stages emit through their executors/harnesses.

### `outbox`

Exports declared workspace-relative files or directories into the durable run
journal. Paths that escape the workspace fail closed. Missing declared paths
are skipped.

## Placement primitive: `runsOn` (DSL 3.0)

`runsOn` appears on gaggles, tasks, and agentic gates.

| Field | Values |
| --- | --- |
| `os` | `linux`, `windows`, `macOS` |
| `cpu` | Kubernetes quantity such as `2000m` |
| `memory` | Kubernetes quantity such as `4Gi` |
| `disk` | Kubernetes quantity such as `20Gi` |
| `capabilities` | Open runner/toolchain tags; `os=*` tokens are rejected in DSL 3.0 |
| `restrictions` | `network:none`, `network:allowlist`, `fs:readonly-except-workspace`, `tmp:ephemeral`, `env:default-deny` |

Task/gate placement is merged with the gaggle floor. Automated and human gates
are control-plane states and cannot declare `runsOn`. An agentic gate that
declares it must include both `cpu` and `memory`.

## Repository handoff primitives (DSL 3.0)

| Field | Description |
| --- | --- |
| `repoFrom` | One producer stage name or a list of possible producers whose run-branch state the stage consumes. |
| `commitsRepo` | Marks a deterministic command/script as a producer that advances the run branch. |

Agentic writable-repository stages and known ref-advancing built-ins are
classified as producers automatically. Other deterministic stages must set
`commitsRepo: true` when they commit.

## Parallel primitive

```yaml
parallels:
  - name: inspect
    failurePolicy: continue_on_error
    branches:
      - name: security
        start: review-security
      - name: performance
        start: review-performance
    join: collate
    maxConcurrentBranches: 2
```

| Field | Required | Description |
| --- | --- | --- |
| `name` | yes | State name addressable by workflow transitions. |
| `failurePolicy` | yes | `fail_fast`, `all_or_nothing`, or `continue_on_error`. |
| `branches` | yes | At least two statically declared `name`/`start` arms. |
| `join` | yes | State run once after all successful/accepted branch settlement. |
| `onFailure` | for `fail_fast` and `all_or_nothing` | Failure target; forbidden for `continue_on_error`. |
| `branchTimeoutSeconds` | no | Positive bound applied to each branch. |
| `maxConcurrentBranches` | no | Maximum simultaneous branches; defaults to `1`. |

Successful branch paths end at `@join`; a branch may instead terminate the run
through `@abort` or `@escalate`. Human gates and writable repository workspaces
are not permitted inside parallel branches.

## Run-control primitives

Run controls may be declared at instance, gaggle, or workflow scope, with the
narrower scope overriding the broader one.

| Field | Description |
| --- | --- |
| `maxRepasses` | Bounds how often gates may route back to an already completed stage. A non-human gate may override it. |
| `stalledRunTimeout` | Positive Go duration after which a silent running journal is escalated. |
| `maxRunDuration` | Positive Go duration bounding total run age; empty disables this bound. |

Readiness fields (`maxConcurrentRuns`, `desiredConcurrentRuns`,
`maxRunsPerHour`, `maxRunsPerDay`, `maxChainDepth`, and `maxOpenPRs`) govern
admission rather than stage execution.
