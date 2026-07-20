# Goobers DSL terminology

Use these definitions when explaining a generated config.

| Term | Meaning |
|---|---|
| **Gaggle** | A siloed workforce inside an instance. It owns its goobers and workflows, targets a project codebase and one backlog, and scopes credentials, work, journals, and telemetry. |
| **Goober** | A role-specialized AI worker defined by YAML plus instruction markdown. A goober never schedules itself; an agentic task invokes it through a harness. |
| **Workflow** | A deterministic state machine that belongs to one gaggle. It declares when a run is eligible and connects task and gate states into a process. |
| **Task** | A state that performs work. A deterministic task runs a command; an agentic task invokes a goober. Architecture documents may call a task a **stage**; the terms are equivalent. |
| **Gate** | A state that validates a result and branches by outcome. It has exactly one automated, agentic, or human evaluator. A gate decides where the workflow goes; it does not perform the workflow's primary work. |
| **Trigger** | A declared event that makes a workflow eligible to run, such as manual invocation, a schedule, a backlog item, or a signal. Readiness limits must also allow the run. |
| **Capability** | A canonical, scoped grant such as `repo:read` or `github:issues:write`. Capabilities are default-deny: a task receives only what it declares, and an agentic task cannot exceed its goober's grants. |

## How the terms fit

```text
instance
  -> gaggle (project + singleton backlog + isolation)
       -> goober (role + instructions + harness + grants)
       -> workflow (trigger + readiness + state graph)
            -> task (does work; agentic tasks invoke a goober)
            -> gate (evaluates and branches)
```

A trigger does not execute a goober directly. It makes a workflow eligible;
the runner starts the workflow, advances its task and gate states, and records
the run in the journal.
