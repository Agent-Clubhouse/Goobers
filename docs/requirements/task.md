# Spec: Task

> Status: **Draft** · Derives from `../VISION.md` §6, §7 · Area prefix: `TSK`

## Purpose

A **Task** is a single state in a workflow — the smallest unit of work the engine tracks.
It can be **deterministic** (code-driven) or **agentic** (executed by a goober). It has
defined inputs, work to be done, and a goal.

## Model

- A Task is a **state** in a workflow's state machine, with defined **input states**
  (preconditions), the **work** to perform, and a **goal** (intended outcome).
- **Deterministic tasks** run code (scripts, checks, integrations). **Agentic tasks**
  invoke a goober, handing it a context/data block + the task definition.
- A Task reports completion + result back to the workflow so the engine can advance:
  agentic tasks via the goober's completion tool (`GBO-013`); deterministic tasks via
  their return.
- A Task is *not* a Gate. A Task does work; a **Gate** validates and branches (see Gate
  spec). A task may declare expected outputs/postconditions that gates can check.

## Requirements

### Definition
- **TSK-001 (MUST):** A Task MUST declare its input states (preconditions), the work to be
  done, and its goal.
- **TSK-002 (MUST):** A Task MUST be exactly one of: deterministic (code-driven) or
  agentic (goober-executed).
- **TSK-003 (SHOULD):** A Task SHOULD declare expected outputs/postconditions so
  downstream gates can validate them.

### Agentic tasks
- **TSK-010 (MUST):** An agentic Task MUST target a specific goober (definition) within
  the gaggle.
- **TSK-011 (MUST):** An agentic Task MUST deliver the context/data block + task
  definition to the goober via the invocation hook (`GBO-012`).
- **TSK-012 (MUST):** An agentic Task MUST be considered complete only when the goober
  calls the designated completion tool/method (`GBO-013`).

### Deterministic tasks
- **TSK-020 (MUST):** A deterministic Task MUST run code/scripts/integrations without a
  goober and return a structured result the engine can act on.

### Lifecycle, telemetry & recovery
- **TSK-030 (MUST):** Every Task (both kinds) MUST emit telemetry — start/stop, logs,
  and outputs — to the goober-run telemetry store (`GBO-020`).
- **TSK-031 (MUST):** Task failure or timeout MUST be surfaced to the workflow engine for
  retry/recovery (mechanism owned by Workflow/Scheduler).
- **TSK-032 (SHOULD):** A Task SHOULD be idempotent or safely retryable, given retries are
  a first-class engine capability (`WF-021`).

## Relationships

- Part of → a **Workflow** (it is a state in the machine).
- Agentic tasks invoke → a **Goober**.
- Followed/guarded by → **Gates** (which validate outputs and branch).
- Emits → telemetry into the **Telemetry** store.

## Open questions

- **TSK-Q1:** ~~Common context schema~~ **Resolved (shape):** a standard JSON **invocation
  envelope** — `taskId, workflowId, runId, gaggle, item/trigger payload, goal, repoRef,
  upstreamOutputs, limits` + a task-specific `inputs` blob. *(Remaining: finalize fields.)*
- **TSK-Q2:** ~~Standard result contract~~ **Resolved (shape):** a standard **result
  envelope** — `status (success|failed|needs-escalation), outputs, artifacts (e.g. PR
  links), summary, metrics, error?`. Gates and telemetry depend on this shape.
  *(Remaining: finalize fields.)*
- **TSK-Q3:** Can a single task fan out to multiple goober runs (parallelism within a
  task) or is parallelism only expressed at the workflow level?
