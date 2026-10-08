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
are skipped. One attempt may export at most 200 files and 64 MiB in aggregate.
The runner measures the complete candidate set before reading or publishing it,
so a rejected oversized batch is never partially accepted.
Durable paths include both the retry attempt and an immutable execution
occurrence. Gate repasses may reuse attempt 1, but never reuse an occurrence or
overwrite the bytes referenced by an earlier `artifact.recorded` event.

Outbox collection happens after the command reports its result. If collection
fails, the journal preserves that command status/error in an
`outbox.export.failed` runner annotation on the same stage and attempt, then
records a normal failed `stage.finished` with error code
`outbox_export_failed`. The workflow fails even when the task declares
`continueOnError`: promised evidence that was not durably recorded cannot be
treated as a successful or tolerated result. Size failures include the
effective limits, measured file count and aggregate bytes, and the three
largest observed files.

For large validation corpora, export a compact manifest containing each
outcome plus stable hashes and explicit omission/truncation markers. Put raw
logs in durable artifact storage and reference them from that manifest; a
runner-local path alone is not portable evidence.

Outbox is export-only. Later stages in the same run do not receive outbox
files, and untracked workspace files do not survive between stages. To pass a
file from one stage to a later stage, see
[Stage-to-stage fan-in](#stage-to-stage-fan-in).

#### Local outbox mirror (`outboxMirrorPath`)

`outboxMirrorPath` can be set on a task, a workflow, or a gaggle. It names an
absolute or `~/` root on the runner host that receives a copy of each exported
outbox file after the copy is written to the journal. The most specific value
wins: the task value overrides the workflow value, and the workflow value
overrides the gaggle default. The gaggle value only applies to workflows and
tasks that do not set their own.

Each mirrored file is written to:

```text
<outboxMirrorPath>/<run-id>/<stage>/attempt-<N>/occurrence-<S>/<declared workspace-relative path>
```

- `<stage>` is the task name. Parallel-branch stages use the same layout.
- `attempt-<N>` is the retry attempt and starts at 1. A gate repass that
  re-enters the task starts again at `attempt-1`.
- `occurrence-<S>` is the journal sequence number of the export batch. It is
  unique within the run, so a repass never overwrites an earlier copy. Use
  the run journal's `artifact.recorded` events to find a specific occurrence,
  not the directory order.
- The declared workspace-relative path is kept as-is, including hidden
  directories. For example, `outbox: [.goobers/panel]` writes
  `.../occurrence-<S>/.goobers/panel/<name>.md`.

The mirror path is the same as the journal's
`artifacts/outbox/<stage>/attempt-<N>/occurrence-<S>/...` path with the
`artifacts/outbox/` prefix replaced by `<run-id>/`. The journal is the source
of truth, and the mirror is a convenience copy for host tools. Reading the
mirror back into a later stage is outside the run contract.

### Stage-to-stage fan-in

Use artifacts, not outbox, when one stage consumes files that earlier stages
produced, such as a review panel whose governor reads every reviewer's
report. Each producer publishes one artifact with `inputs.artifactFile` (or
`inputs.artifactManifestFile`). Its artifact pointers, named
`<stage>.artifact[<i>]`, are added to the context passed to later linear
stages and to parallel joins. Agentic consumers get these artifacts in their
workspace under `.goobers/context/` and through the `goobers-io` read tools
(see [Goobers IO MCP](../../guides/goobers-io-mcp.md)). Use `contextFrom` on
the consumer to limit what it receives to the named producers:

```yaml
- name: review-security
  type: agentic
  goober: reviewer
  goal: Review the change through a security lens.
  workspace: repo-readonly
  inputs:
    artifactFile: report.md
  next: review-performance
# ... one stage per reviewer ...
- name: govern
  type: agentic
  goober: governor
  goal: Adjudicate the panel's reports.
  workspace: repo-readonly
  contextFrom: [review-security, review-performance]
```

The reports are stored in the run journal and never committed, so they do not
change the diff the panel is reviewing.

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
| `maxConcurrentBranches` | no | Maximum simultaneous branches; defaults to `1`, so branches run sequentially. Validation warns when it is unset; set it, even to `1`, to make the schedule explicit. |

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

`maxRunDuration` requires daemon-backed execution. Without a live daemon,
`goobers run` rejects the selected workflow before dispatch if its effective
limit is set, including inherited limits and `--no-wait` runs. Start
`goobers up` for the instance and submit through the daemon instead.
`--no-api` file delegation to a live daemon remains supported.

Readiness fields (`maxConcurrentRuns`, `desiredConcurrentRuns`,
`maxRunsPerHour`, `maxRunsPerDay`, `maxChainDepth`, and `maxOpenPRs`) govern
admission rather than stage execution. `maxOpenPRs` counts the open pull
requests under the workflow's run-branch namespace in the gaggle's project
repository, on GitHub and Azure DevOps alike, and ignores PRs labelled
`goobers:merge-escalated` in any casing. Until a count has been read, and
whenever a read fails, admission is not held back by the cap.
