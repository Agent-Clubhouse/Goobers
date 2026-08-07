# Workflow-authoring guide

This guide explains how to write Goobers workflow, goober, and gaggle
definitions from scratch. It uses the `config-examples/gaggles/acme-web`
definitions as runnable references throughout. Read
[the goober-authoring guide](goober-authoring.md) alongside this one for the
agentic side.

## Core concepts

A **gaggle** names a target project and its backlog. It owns one namespace and
provides the project context every workflow in the gaggle operates on.

A **workflow** is a directed state graph of **tasks** and **gates**. A task
does work; a gate branches on its result. Every workflow belongs to exactly one
gaggle.

A **goober** is a named agentic worker. A workflow's agentic task names the
goober that the runner invokes.

A **stage** is a task or gate at runtime. The runner executes stages one at a
time, records every input and output in the run journal, and never lets a stage
reach into another stage's state directly — stages exchange envelopes and
artifact pointers only (see [stage-contract.md](../stage-contract.md)).

## DSL structure

Every definition file follows the Kubernetes-style resource shape:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Gaggle | Goober | Workflow
dslVersion: "1.4"   # Workflow only; use goobers versions --json for supported values
metadata:
  name: lowercase-dns-style-name
spec: {}
```

Schema validation is strict: unknown fields are rejected. Use
`goobers validate --source-tree <path>` to catch errors before the daemon
starts.

## Gaggle

The gaggle declares the target repository, backlog source, connection
references, and isolation namespace. The
[`config-examples/gaggles/acme-web/gaggle.yaml`](../../config-examples/gaggles/acme-web/gaggle.yaml)
definition is the canonical runnable reference.

### Minimal gaggle

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: my-gaggle
spec:
  displayName: My Gaggle
  project:
    provider: github
    owner: my-org
    name: my-repo
    branch: main
    connectionRef: repo-token
  backlog:
    provider: github
    project: my-org/my-repo
    labels:
      - goobers:approved
    connectionRef: repo-token
  isolation:
    namespace: gaggle-my-gaggle
```

Connection names (`connectionRef` values) must resolve to entries under
`manifest.yaml`'s `spec.connections`. Never put token values in a gaggle file;
credentials are injected through `instance.yaml`.

### Optional gaggle fields

| Field | Purpose |
|---|---|
| `ciCommand` | Override the Go-default `["make", "ci"]` for non-Go stacks (e.g. `["npm", "run", "ci"]`). |
| `requiredCapabilities` | Tokens like `node@20` or `os=linux`; checked at schedule time and probed at run time. |
| `isolation.identityRef` | Workload identity for credential isolation between gaggles sharing a repository. |

## Workflow triggers

Every workflow must declare at least one trigger.

| Type | When to use | Key fields |
|---|---|---|
| `manual` | First acceptance run; no autonomous scheduling yet. | None; must be the only trigger. |
| `schedule` | Autonomous, time-based execution. | `schedule`: a cron expression (`"0 * * * *"`) or descriptor (`"@daily"`). |
| `backlog-item` | Local daemon poll-dispatch on eligible items. | `selector`: map of required backlog labels. |
| `signal` | External event-driven trigger. | `signal`: named signal string. |

For the first autonomous V0 backlog loop, use a `schedule` trigger and make the
first task claim one eligible item:

```yaml
triggers:
  - type: schedule
    schedule: "3,18,33,48 * * * *"
readiness:
  maxConcurrentRuns: 1
  maxRunsPerHour: 8
```

`readiness` limits protect against runaway scheduling. Start conservatively with
`maxConcurrentRuns: 1`. Add `maxRunsPerDay` or `maxRunsPerHour` as explicit
ceilings once the first acceptance run completes.

## Tasks

A workflow's `tasks` list declares the work states. The `start` field names the
first task or gate.

### Deterministic task

Runs a host command in an isolated worktree. The command writes a flat JSON
result file; the executor merges its scalar fields into `outputs`.

```yaml
- name: query-backlog
  type: deterministic
  goal: Claim one approved backlog item.
  run:
    command: ["goobers", "backlog-query", "--claim"]
  inputs:
    trustLabel: "goobers:approved"
    maxItems: "1"
    resultFile: "claimed-item.json"
  capabilities:
    - github:issues:write
  expectedOutputs:
    - claimed-item
  next: implement
```

Required fields: `name`, `type: deterministic`, `goal`, `run.command`. Do not
set `goober` on a deterministic task. `next` names the downstream task or gate;
omit it on a terminal task (or set it to `@abort` / `@escalate` explicitly for
non-success terminals).

`inputs` values are strings. `expectedOutputs` is a hint for downstream
readers; it does not gate execution. Capabilities listed here must also appear
in the goober's grants when an agentic stage later in the same workflow
references them.

#### Inline script task

Use `run.script` instead of `run.command` when the check fits in a short POSIX
or CMD script and needs no project build context:

```yaml
- name: check-label
  type: deterministic
  goal: Check whether a required label is present.
  run:
    script: |
      set -eu
      case ",${GOOBERS_INPUT_LABELS}," in
        *",${GOOBERS_INPUT_REQUIREDLABEL},"*) allowed=true ;;
        *) allowed=false ;;
      esac
      printf '{"allowed":%s}\n' "$allowed" >"$GOOBERS_INPUT_RESULTFILE"
    workspace: scratch
  inputs:
    labels: "ready,security-reviewed"
    requiredLabel: "security-reviewed"
    resultFile: "policy-result.json"
  requiredCapabilities: ["os=linux"]
  expectedOutputs: [allowed]
  next: label-policy
```

`workspace: scratch` declares no project worktree dependency. Do not set both
`command` and `script`; validation rejects that ambiguity. See
[custom-stage-cookbook.md](custom-stage-cookbook.md) for a complete walkthrough.

#### Built-in stage kinds

Some deterministic tasks are handled by executor built-ins rather than shelling
out. These are selected by `inputs.kind`:

| Kind | Command placeholder | What it does |
|---|---|---|
| `backlog-query` | `["goobers", "backlog-query", "--claim"]` | Claims one or more trust-labeled backlog items; writes a result file the curator or implementer consumes as context. |
| `ci-poll` | `["goobers", "ci-poll"]` | Polls the PR's CI checks until they conclude; sets `ciStatus` to `passing`, `failing`, or `pending`. |
| `telemetry-query` | `["goobers", "telemetry-query"]` | Queries the local telemetry rollup; requires `telemetry:read`. |

The `command` placeholder is required by the schema even though the executor
never shells it out for these kinds.

#### Artifact pointers between stages

A task can declare `inputs.artifactFile: <name>` to produce a rich output (a
report, generated document) as a named artifact. The next task receives it
automatically as a context pointer readable via the `goobers-io` MCP tools —
no extra YAML is needed. See [goobers-io-mcp.md](goobers-io-mcp.md) for the
complete mechanics.

For scalar handoffs (one task outputs `prNumber`, the next consumes it), use
`inputsFrom`:

```yaml
- name: ci-poll
  inputsFrom:
    prNumber: prNumber   # reads open-pr's declared prNumber output
```

### Agentic task

Invokes a named goober in an isolated worktree. The runner attaches context
pointers from earlier stages listed under `contextFrom`.

```yaml
- name: implement
  type: agentic
  goober: implementer
  minimumIntegrity: maintainer
  contextFrom:
    - query-backlog
    - implement
    - review
  goal: >-
    Implement the claimed issue end to end. Read the issue from context,
    plan, implement, verify with fast targeted tests, and commit.
  capabilities:
    - repo:push
  policyActions:
    - modify-repository
  retry:
    maxAttempts: 2
    backoffSeconds: 15
  onTimeout: salvage
  expectedOutputs:
    - changed-files
  next: review
```

Required fields: `name`, `type: agentic`, `goal`, `goober`. Do not set `run`
on an agentic task. Every capability listed here must also appear in the
referenced goober's `spec.capabilities`.

`minimumIntegrity: maintainer` restricts the task to only consume artifacts
that originate from maintainer-trusted sources (SEC-047). Lower it to
`unapproved` when provider-authored evidence (CI failure details, forge
metadata) needs to reach the task — use a separate remediation task for that
rather than widening the primary implementation task's trust boundary.

`onTimeout: salvage` advances the run to the next state even when the agentic
session times out, provided the committed diff is non-empty. This avoids
discarding real work on slow sessions.

## Gates

A gate evaluates the preceding task's outputs and routes the run along a named
branch.

```yaml
gates:
  - name: review
    evaluator: agentic
    agentic:
      goober: reviewer
    branches:
      pass: local-ci
      needs-changes: implement
      fail: park-needs-human
      escalate: park-escalated
```

### Automated gate

```yaml
  - name: ci-gate
    evaluator: automated
    automated:
      check: ci-status
      params:
        equals: "passing"
    branches:
      pass: close-out
      fail: remediate-ci
      escalate: park-escalated
      timeout: park-escalated
```

Supported automated checks: `status-equals`, `output-equals`,
`output-not-equals`, `output-numeric-gte`, `output-numeric-lte`,
`output-numeric-lt`, `output-matches`, `ci-status`, `land-outcome`,
`queue-outcome`, `failure-class`.

All gate branch targets must name existing tasks, other gates, or the reserved
terminals `@abort` and `@escalate`. A gate's `fail` branch is a label, not a
failed workflow — `fail: report-clean` means "route to report-clean when the
predicate is false".

## Canonical capabilities

Grant the minimum set. For each capability used in a task, list it in both the
task's `capabilities` field and in the referenced goober's `spec.capabilities`.

| Capability | Grant |
|---|---|
| `repo:read` | Read-only target-repository checkout. |
| `repo:push` | Push the run branch to the target repository. |
| `github:issues:read` | Query GitHub issues without mutation authority. |
| `github:issues:write` | Query, create, label, close, or comment on GitHub issues. |
| `github:milestones:write` | Assign existing GitHub milestones to selected issues. |
| `github:pr:write` | Open, inspect, update, or close GitHub pull requests. |
| `github:pr:review` | Submit provider-native pull-request reviews. |
| `github:pr:merge` | Merge a GitHub pull request. |
| `github:branch:delete` | Delete a remote GitHub branch. |
| `telemetry:read` | Read the Goobers telemetry rollup. |
| `journal:read` | Resolve evidence from another run's journal. |
| `agent:model` | Supply an agentic harness with its model credential. |

## Readiness and run budgets

```yaml
readiness:
  maxConcurrentRuns: 1    # safe starting value; raise after acceptance run
  maxRunsPerHour: 8       # hourly ceiling as a runaway guard
  maxRunsPerDay: 4        # daily ceiling; use instead of cadence math when simpler
```

`runConditions.workflowDailyBudgets` in `instance.yaml` sets a daily ceiling
per workflow name; `readiness` limits inside the workflow definition apply to
that workflow alone.

## Putting it together: the V0 implementation workflow

The
[`config-examples/gaggles/acme-web/workflows/implementation.yaml`](../../config-examples/gaggles/acme-web/workflows/implementation.yaml)
is the canonical V0 loop: claim → implement → review gate → local-CI gate →
push → open-PR → CI-poll gate → close-out, with bounded repasses back to
implement on failures and a park path for escalations.

The
[`internal/instance/starter/gaggles/example/workflows/default-implement.yaml`](../../internal/instance/starter/gaggles/example/workflows/default-implement.yaml)
is the minimal single-task variant produced by `goobers init`. Start there for
a new project, then extend toward the full reference once the basic loop works.

## Validate

Always validate before starting the daemon:

```sh
# Checked-in config source tree
goobers validate --source-tree "$GOOBERS_CONFIG_SOURCE"

# Initialized runtime instance
goobers validate "$GOOBERS_INSTANCE"
goobers validate --check-harness "$GOOBERS_INSTANCE"
```

The validator catches broken state references, unreachable states, invalid
schedules, incomplete gate outcomes, unknown capabilities, and task grants that
exceed goober grants. See [failure-mode-cookbook.md](failure-mode-cookbook.md)
for a catalog of common validation errors and how to fix them.
