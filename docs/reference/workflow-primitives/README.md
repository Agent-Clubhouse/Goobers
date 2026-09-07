# Workflow primitive reference

This reference answers two questions for workflow authors:

1. Which built-in values may a workflow name?
2. Where may each value appear, and what parameters and outcomes does it use?

The YAML schema describes field shapes, while this reference describes the
closed vocabularies and composition rules behind those fields. Use the
reference that ships with the same Goobers release as the instance you are
configuring. `goobers features --json` and
[`docs/feature-matrix.md`](../../feature-matrix.md) report availability for a
specific DSL version.

## Primitive categories

| Category | YAML location | Reference |
| --- | --- | --- |
| Trigger types | `spec.triggers[].type` | [Triggers](triggers.md) |
| Task types and execution kinds | `spec.tasks[].type`, `spec.tasks[].inputs.kind` | [Tasks and stages](tasks-and-stages.md) |
| Built-in stage commands | `spec.tasks[].run.command` | [Stage commands](stage-commands.md) |
| Gate evaluators and checks | `spec.gates[].evaluator`, `spec.gates[].automated.check` | [Gates and checks](gates-and-checks.md) |
| Policy actions | task and Goober `policyActions` | [Policy actions](policy-actions.md) |
| Credential capabilities | task and Goober `capabilities` | [Capabilities](capabilities.md) |
| Workspaces, placement, data flow, and terminals | task, gate, and transition fields | [Graph and execution](graph-and-execution.md) |

## Placement model

Primitive names are typed. They are not imports and cannot be moved to an
arbitrary part of the document:

```yaml
spec:
  triggers:
    - type: schedule
      schedule: "@every 15m"
  start: query
  tasks:
    - name: query
      type: deterministic
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        resultFile: "claimed-item.json"
      capabilities: [github:issues:write]
      policyActions: [claim-backlog-items]
      next: claimed
  gates:
    - name: claimed
      evaluator: automated
      automated:
        check: output-not-equals
        params:
          key: claimed-item
          equals: ""
      branches:
        pass: implement
        fail: "@abort"
```

In that example:

- `schedule` is valid only as a trigger type.
- `deterministic` is valid only as a task type.
- `backlog-query` is a built-in command invoked by a deterministic shell
  stage.
- `output-not-equals` is valid only as an automated gate check.
- `github:issues:write` is a credential capability.
- `claim-backlog-items` is a policy action.
- `@abort` is a reserved graph terminal.

## Cross-cutting declarations

Some declarations deliberately appear at more than one scope:

| Declaration | Valid scopes | Rule |
| --- | --- | --- |
| `capabilities` | Goober, task | An agentic task's grants must be a subset of its Goober's grants. |
| `policyActions` | Goober, task | An agentic task must declare every unconditional action prescribed by its Goober. |
| `retry` | task, automated evaluator, agentic evaluator | The containing task or evaluator owns the retry budget. |
| `timeoutSeconds` | task and evaluator configurations | The innermost declaration bounds that attempt. |
| `workspace` | task, deterministic `run`, agentic evaluator | `run.workspace` wins when a deterministic task declares both task and run workspace. |
| `runsOn` | gaggle, task, agentic gate in DSL 3.0 | Stage placement is merged with the gaggle floor; automated and human gates are control-plane states. |
| state targets | workflow `start`, task `next`, gate `branches`, parallel fields | A target names a task, gate, parallel, or an allowed reserved target. |

Similar names do not always mean the same vocabulary. In particular:

- `capabilities` are credential/authority grants from the closed capability
  registry.
- DSL 2.0 `requiredCapabilities` and DSL 3.0
  `runsOn.capabilities` are open runner/toolchain placement tags.
- `spec.requires.capabilities` describes provider-contract requirements, not
  stage credentials.

## Authoritative sources

| Vocabulary | Source of truth |
| --- | --- |
| Shell-invokable built-in commands | `internal/builtincmd/builtincmd.go` |
| Deterministic stage kinds | `internal/executor/dispatch.go` and registered executors |
| Automated gate checks | `internal/gate/automated.go` |
| Gate outcome validation | versioned workflow compiler |
| Policy actions and required capabilities | versioned `internal/workflow/*/policy_actions.go` |
| Credential capabilities | `internal/capability/capability.go` |
| Trigger, task, evaluator, workspace, and parallel enums | `api/v1alpha1/workflow_types.go` |
| DSL-version availability | workflow feature registry and `docs/feature-matrix.md` |

Run `goobers validate --json <instance>` after editing YAML. JSON Schema catches
shape errors; compilation additionally catches unknown primitives, invalid
placement, missing gate outcomes, capability/action mismatches, and broken
graph targets.
