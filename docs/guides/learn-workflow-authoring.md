# Learn Goobers: author, test, and debug a workflow

This tutorial continues after the
[credential-free quickstart](quickstart.md). It teaches how workflow YAML
becomes a validated state machine, how to add deterministic and agentic work,
how to branch through gates, and how to diagnose a run without editing runtime
state by hand.

Use the [workflow primitive reference](../reference/workflow-primitives/README.md)
when you need the complete list of accepted primitive names, parameters,
outcomes, and placement rules.

## What you will build

You will start from the release-matched `quickstart` template and inspect or
add:

1. a manual trigger;
2. a deterministic built-in stage;
3. an agentic implementation task;
4. an agentic review gate;
5. an automated local-CI gate with a bounded repass;
6. explicit capabilities, policy actions, data handoffs, and terminal routes.

The final graph separates decisions from mutations:

```text
query-backlog -> implement -> review-gate
                    ^          | pass
                    |          v
                    +------ local-ci -> local-ci-gate
                                      | pass
                                      v
                                  push-branch -> open-pr -> complete
```

`review-gate.needs-changes` and `local-ci-gate.fail` return to `implement`.
Irrecoverable review failure routes to `@abort`.

## Prerequisites

- A Goobers binary from the release whose YAML you are authoring.
- A durable directory outside an ephemeral agent or hosted workspace.
- No provider or model credential is required to scaffold, edit, validate, or
  inspect the graph.
- Running the complete quickstart against GitHub additionally requires the
  disposable-repository setup in the [quickstart](quickstart.md#2-graduate-to-the-token-bearing-quickstart-template).

The examples below use `goobers` from `PATH`. Replace it with `bin/goobers` when
working from a source checkout.

## 1. Materialize a release-matched authoring fixture

Create a disposable instance from the versioned template:

```sh
goobers init --template=quickstart ./learn-goobers
goobers validate --json ./learn-goobers
```

The workflow is:

```text
learn-goobers/config/gaggles/example/workflows/quickstart.yaml
```

The related Goobers and their instructions are under:

```text
learn-goobers/config/gaggles/example/goobers/
```

If you want a checked-in configuration source rather than a runnable instance,
materialize the same fixture with:

```sh
goobers init --template=quickstart --source-tree ./learn-goobers-config --json
goobers validate --source-tree --json ./learn-goobers-config
```

The template is preferable to copying YAML from the website: it is embedded in
the binary, release-matched, and exercised by repository validation tests.

## 2. Read the resource envelope

Open `quickstart.yaml`. The outer fields identify the resource and language:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: quickstart
spec:
  gaggle: example
```

`apiVersion` selects the resource schema family. `dslVersion` selects workflow
language behavior and feature availability. Do not copy a field from a newer
example into an older DSL document without checking:

```sh
goobers versions --json
goobers features --json
```

The workflow compiler pins the normalized definition and graph into each run.
Editing the live config affects later runs; it does not rewrite an in-flight
run's graph.

## 3. Choose how the workflow starts

The template begins safely with an explicit-only trigger:

```yaml
triggers:
  - type: manual
```

A manual trigger must be the only trigger. Start it with:

```sh
goobers run quickstart ./learn-goobers
```

Do not replace it with an autonomous schedule until the workflow has bounded
readiness and a first stage that safely claims or selects work:

```yaml
triggers:
  - type: schedule
    schedule: "3,18,33,48 * * * *"
readiness:
  maxConcurrentRuns: 1
  maxRunsPerHour: 8
```

See [Trigger primitives](../reference/workflow-primitives/triggers.md) for
manual, schedule, backlog-item, signal, and webhook parameters.

## 4. Add a deterministic built-in stage

The first state claims one trusted backlog item:

```yaml
start: query-backlog
tasks:
  - name: query-backlog
    type: deterministic
    goal: Claim the first approved tutorial issue.
    run:
      command: ["goobers", "backlog-query", "--claim"]
    inputs:
      trustLabel: "goobers:approved"
      requireLabels: "goobers:ready"
      maxItems: "1"
      resultFile: "claimed-item.json"
    capabilities:
      - github:issues:write
      - github:pr:write
    policyActions:
      - claim-backlog-items
    expectedOutputs:
      - claimed-item
    next: implement
```

This declaration contains several distinct contracts:

| YAML | Contract |
| --- | --- |
| `type: deterministic` | The runner executes code rather than invoking a Goober. |
| `run.command` | Selects the built-in `backlog-query` command and its `--claim` mode. |
| `inputs` | Supplies string-valued invocation parameters. |
| `capabilities` | Supplies credential authority. |
| `policyActions` | Authorizes the external mutation decision. |
| `expectedOutputs` | Documents outputs on which later states rely. |
| `next` | Adds the graph edge to `implement`. |

The command, capability, and policy action are separate primitives. Declaring
only one does not imply the others. The compiler rejects a policy-bearing
built-in whose complete action/capability contract is missing.

## 5. Add an agentic task

The implementation state delegates work to the `implementer` Goober:

```yaml
- name: implement
  type: agentic
  goober: implementer
  goal: Implement the claimed tutorial issue and commit the change.
  workspace: repo
  capabilities:
    - repo:push
    - agent:model
  policyActions:
    - modify-repository
  retry:
    maxAttempts: 2
    backoffSeconds: 15
  onTimeout: salvage
  next: review-gate
```

The referenced Goober must exist, list this workflow in `spec.workflows`, and
grant every task capability. A task can narrow its Goober's authority but
cannot expand it.

`workspace: repo` gives the stage a writable run-branch worktree.
`onTimeout: salvage` lets a timed-out agentic attempt continue only when it
left a viable committed diff. Use the default `fail` behavior when partial
committed work is not a valid result.

## 6. Turn review into an agentic gate

The starter template uses an agentic task for a simple advisory review. A
review that controls flow belongs in a gate:

```yaml
gates:
  - name: review-gate
    evaluator: agentic
    agentic:
      goober: reviewer
      workspace: repo-readonly
      timeoutSeconds: 900
      retry:
        maxAttempts: 2
        backoffSeconds: 15
    branches:
      pass: local-ci
      needs-changes: implement
      fail: "@abort"
```

An agentic gate has a closed outcome vocabulary: `pass`, `needs-changes`, and
`fail`. Every outcome needs a branch. The reviewer may inspect and decide, but
an agentic gate cannot declare policy actions; route provider mutations to a
task.

The `needs-changes` edge is a repass. Bound repeated returns at workflow scope:

```yaml
runControls:
  maxRepasses: 2
```

Without a bounded repass policy, a noisy reviewer can create an expensive
cycle. When the budget is exhausted, the runner escalates rather than silently
continuing.

## 7. Gate deterministic CI

A failed task normally terminates the run before a gate can inspect it.
`continueOnError: true` keeps the failure visible and advances to its gate:

```yaml
- name: local-ci
  type: deterministic
  goal: Run the repository's local CI-equivalent.
  run:
    command: ["make", "ci"]
    syncBase: true
  retry:
    maxAttempts: 1
  continueOnError: true
  next: local-ci-gate
```

The automated gate reads the normalized status of `local-ci`:

```yaml
- name: local-ci-gate
  evaluator: automated
  automated:
    check: status-equals
    params:
      equals: success
  branches:
    pass: push-branch
    fail: implement
```

The gate receives the preceding result's `status`, error classification, and
scalar outputs. It does not read the preceding stage's result file directly.
See [Gate evaluator and check primitives](../reference/workflow-primitives/gates-and-checks.md)
for every check's parameters and required branches.

## 8. Keep provider mutation deterministic

Agentic implementation commits locally. Dedicated deterministic stages publish
the result:

```yaml
- name: push-branch
  type: deterministic
  goal: Push the reviewed tutorial change.
  run:
    command: ["goobers", "push-branch"]
  capabilities:
    - repo:push
  policyActions:
    - push-repository-branch
  next: open-pr

- name: open-pr
  type: deterministic
  goal: Open the tutorial pull request.
  run:
    command: ["goobers", "open-pr"]
  inputs:
    resultFile: "pr-result.json"
  capabilities:
    - provider:pr:write
  policyActions:
    - open-or-update-pr
  expectedOutputs:
    - pull-request-url
    - prNumber
    - opened
```

No `next` on `open-pr` means successful completion. Provider mutation stays in
small deterministic stages whose capabilities and policy actions can be
audited independently from model behavior.

## 9. Pass data deliberately

Use the narrowest channel matching the data:

| Data | Declaration |
| --- | --- |
| Static stage configuration | `inputs` |
| Scalar output consumed by another task | `inputsFrom` |
| Rich artifact or verdict context | `contextFrom` and artifact pointers |
| Durable non-repository files | `outbox` |
| Repository state in DSL 3.0 | `repoFrom` |

For example, pass the PR number emitted by `open-pr` into a later `ci-poll`
stage:

```yaml
inputsFrom:
  prNumber: open-pr.prNumber
```

The qualified source makes the edge auditable. A missing declared output fails
closed instead of becoming an empty input.

## 10. Validate and inspect compilation

Validate after each coherent edit:

```sh
goobers validate --json ./learn-goobers
```

After replacing the template repository placeholders, use strict mode before
submitting a change:

```sh
goobers validate --strict --json ./learn-goobers
```

The untouched quickstart intentionally reports placeholder warnings for
`your-org/your-repo`; strict mode promotes those warnings to errors until the
instance targets a real disposable repository.

Validation performs more than schema checking. It compiles the workflow and
reports:

- unknown or misplaced primitives;
- task/Goober capability mismatches;
- missing policy actions and capabilities;
- dangling or unreachable graph states;
- gate outcomes without branches and impossible branch names;
- invalid schedules, workspaces, runner placement, and repository handoffs;
- unsupported or preview features for the selected DSL version.

Inspect the normalized graph:

```sh
goobers workflow show quickstart ./learn-goobers
goobers workflow show --dot quickstart ./learn-goobers
```

The text view is fastest for reviewing state kinds and transitions. The DOT
view is useful for larger repass loops and fan-out/fan-in graphs.

### Compile-time versus run-time decisions

| Compile time | Run time |
| --- | --- |
| Primitive names and placement | Which trigger fires |
| Graph targets and reachability | Whether readiness admits the run |
| Required gate outcome coverage | Provider data and stage outputs |
| Capability/action compatibility | Credential resolution |
| DSL feature availability | Command, harness, and provider execution |
| Static runner-placement rules | Which eligible runner receives a stage |

Validation proves the declared machine is coherent. It cannot prove that a
credential has live repository permission, a model is authenticated, or a
remote service is healthy.

## 11. Test at the smallest useful layer

### Schema and compilation, no credentials

```sh
goobers validate --json ./learn-goobers
goobers workflow show quickstart ./learn-goobers
```

Use this loop for ordinary YAML editing. Add `--strict` after replacing the
quickstart's repository placeholders.

### Deterministic full-loop behavior, no credentials

The hermetic demo exercises scheduling, tasks, a gate, artifacts, and journal
inspection without a provider or model:

```sh
goobers init --demo ./learn-goobers-demo
goobers run demo ./learn-goobers-demo
goobers trace <run-id> ./learn-goobers-demo
```

Native Windows uses the WSL 2 path documented in the
[Windows quickstart](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/quickstart-windows.md)
because the demo requires enforced network isolation.

### Harness preflight

Before a credential-dependent agentic run:

```sh
goobers validate --check-harness ./learn-goobers
```

### Repository/provider preflight

After connecting only a disposable repository:

```sh
goobers validate --check-repos ./learn-goobers
goobers backlog-query --read-only ./learn-goobers
```

Read-only probing should precede any claim, issue mutation, branch push, or PR
creation.

### End-to-end disposable run

Follow the quickstart's repository/token setup, then:

```sh
goobers run quickstart ./learn-goobers
```

Do not use a production backlog as a tutorial fixture.

## 12. Trace source YAML to journal events

For the unmodified quickstart template, compilation produces this linear state
sequence:

```text
query-backlog
  -> implement
  -> review
  -> local-ci
  -> push-branch
  -> open-pr
  -> successful terminal
```

After a run, inspect it with:

```sh
goobers trace --summary <run-id> ./learn-goobers
goobers trace <run-id> ./learn-goobers
goobers trace --transcripts <run-id> ./learn-goobers
```

Correlate each source state with the journal:

1. `run.yaml` pins the workflow identity, definition digest, graph, and
   effective run controls; `run.started` opens the append-only event sequence.
2. Each task attempt emits `stage.started` followed by progress and
   `stage.finished`.
3. The event's `stage` matches the YAML task name.
4. `attempt` distinguishes retries without overwriting history.
5. Outputs and artifact pointers from one stage become declared inputs/context
   for later stages.
6. `run.finished` records the terminal phase and final state.

When gates are present, gate events record the evaluated gate, outcome, and
selected target. A repass creates new stage attempts; it never erases the
failed attempt that caused the loop.

The run uses the workflow definition and graph pinned at `run.started`.
Comparing a trace with newly edited live YAML can therefore be misleading;
inspect the run's pinned identity when behavior differs from the current file.

## 13. Debug common failures

| Symptom | Inspect | Typical correction |
| --- | --- | --- |
| Validation reports an unknown field | JSON pointer in `goobers validate --json`; selected `dslVersion` | Correct the spelling or migrate to a DSL version supporting the field. |
| Unknown built-in command or gate check | [Primitive reference](../reference/workflow-primitives/README.md) | Use a registered primitive; do not shell out to operator commands from a workflow. |
| Dangling or unreachable state | `goobers workflow show`; `start`, `next`, and `branches` | Add the missing state/edge or remove dead YAML. |
| Gate has no branch for an outcome | Gate check outcome table | Declare every producible outcome, including `timeout`, `infra`, or `needs-changes` where applicable. |
| Capability or policy-action error | Task, referenced Goober, and command mode | Add the narrow required grant/action or remove authority the task does not need. |
| Run finds no work | `status`, `trace --summary`, trigger selectors, labels, assignment filters | Compare the live item with every selector and run `backlog-query --read-only`. |
| Claim appears stuck | `status`, `claims list`, run trace | Determine whether the owning run is live before using the documented claim-release flow. Never delete ledger files manually. |
| Agentic stage fails immediately | `validate --check-harness`, stage transcript | Repair harness installation/authentication or missing `agent:model`; do not add provider credentials blindly. |
| Deterministic stage fails | `trace`, stage error code/message, declared workspace and toolchain placement | Reproduce the exact command in an equivalent disposable workspace and fix the command or runner requirements. |
| CI repeatedly repasses | `ci-checks.json`, `ciStatus`, repass count | Fix the reported check, separate infrastructure failures with `failure-class`, and keep `maxRepasses` bounded. |
| Gate takes an unexpected branch | Preceding stage outputs and gate params | Use the check's documented vocabulary; `ci-status` uses `passing`/`failing`, not `success`/`failure`. |
| Run appears stuck | `goobers status --agents --json`, `trace --follow`, daemon status | Distinguish an active agent, a parked human gate, provider polling, and a stalled journal before intervening. |
| Workspace contains unexpected state | Pinned workflow, `workspace`, DSL 3.0 `repoFrom`, workcopy status | Correct the declared handoff; use `goobers workspace reset` only after confirming no live lease. |

Useful commands:

```sh
goobers status --json ./learn-goobers
goobers status --agents --json ./learn-goobers
goobers trace --follow <run-id> ./learn-goobers
goobers trace --verdicts <run-id> ./learn-goobers
goobers claims list ./learn-goobers
```

Prefer the journal and supported recovery commands over manual edits under
`runs/`, `scheduler/`, or `workcopies/`. Those directories are runtime state,
not configuration.

## 14. Production-readiness review

Before turning a tutorial workflow into an autonomous production workflow:

- replace `manual` only after trigger selectors and readiness budgets are
  bounded;
- require a maintainer-controlled trust signal before consuming untrusted
  backlog content;
- grant the minimum task and Goober capabilities;
- declare every externally mutating policy action;
- separate agentic decisions from deterministic provider mutations;
- route every gate outcome explicitly and bound repasses;
- choose `scratch` or `repo-readonly` when writable repository state is not
  needed;
- declare timeouts and realistic retry budgets;
- test provider writes in a disposable repository;
- inspect the compiled graph and a complete journal;
- preserve release/DSL pins while runs are in flight.

For production-oriented complete examples, use
[`config-examples/`](../../config-examples/README.md). They are validated as a
configuration source tree in the repository test suite and demonstrate review,
CI polling, remediation, merge policy, backlog operations, and documentation
workflows.
