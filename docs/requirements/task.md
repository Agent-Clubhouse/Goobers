# Spec: Task

> Status: **Draft** · Aligned to `../ARCHITECTURE.md` (2026-07-12) · Derives from
> `../VISION.md` §6, §7 · Area prefix: `TSK`

## Purpose

A **Task** is a single state in a workflow — the smallest unit of work the runner tracks.
It can be **deterministic** (code-driven) or **agentic** (executed by a goober). It has
defined inputs, work to be done, and a goal. `ARCHITECTURE.md` refers to tasks as
**stages**; the terms are equivalent across the whole doc set (`ARCHITECTURE.md §5`).

## Model

- A Task is a **state** in a workflow's compiled state machine, with defined **input
  states** (preconditions), the **work** to perform, and a **goal** (intended outcome).
  All side effects of a workflow happen inside tasks; the machine itself takes no
  hidden inputs.
- **Deterministic tasks** run arbitrary commands (tests, linters, builders, CI pollers)
  in the task's run environment. **Agentic tasks** invoke a goober's harness with an
  **invocation envelope** (goal, context pointers, capability grants).
- Every Task executes in an **ephemeral, isolated, disposable workspace**. Repo-backed
  tasks receive a fresh working copy of the target repo: a git worktree off the managed
  working copy, run as a local process, at tiers 1–2; the workspace of an ephemeral pod
  at tier 3 (V2). Deterministic tasks may instead request an empty scratch workspace.
- A Task reports completion + result back to the runner so the machine can advance:
  agentic tasks via the goober's **result envelope** (`GBO-013`); deterministic tasks
  via their structured return. Either way the outcome is appended to the **run
  journal** as events, spans, and content-digested artifacts.
- Tasks hand data to one another only through **artifact pointers** (path + digest
  inside the run journal), never through implicit shared state.
- A Task is *not* a Gate. A Task does work; a **Gate** validates and branches (see Gate
  spec). A task may declare expected outputs/postconditions that gates can check.

## Requirements

### Definition
- **TSK-001 (MUST):** A Task MUST declare its input states (preconditions), the work to
  be done, and its goal. *(All tiers)*
- **TSK-002 (MUST):** A Task MUST be exactly one of: deterministic (code-driven) or
  agentic (goober-executed). *(All tiers)*
- **TSK-003 (SHOULD):** A Task SHOULD declare expected outputs/postconditions so
  downstream gates can validate them.

### Agentic tasks
- **TSK-010 (MUST):** An agentic Task MUST target a specific goober (definition) within
  the gaggle. *(All tiers)*
- **TSK-011 (MUST):** An agentic Task MUST deliver the **invocation envelope** (goal,
  context pointers, capability grants) + task definition to the goober's harness
  (`GBO-012`); the harness adapter (first: GitHub Copilot CLI) is a task-level detail
  behind this contract. *(All tiers)*
- **TSK-012 (MUST):** An agentic Task MUST be considered complete only when the goober
  produces the designated **result envelope** (status, outputs, artifact pointers)
  (`GBO-013`). *(All tiers)*

### Deterministic tasks
- **TSK-020 (MUST):** A deterministic Task MUST run commands/scripts/integrations in
  the task's run environment without a goober and return a structured result the
  runner can act on. *(All tiers)*

### Lifecycle, telemetry & recovery
- **TSK-030 (MUST):** Every Task (both kinds) MUST emit telemetry — start/stop, logs,
  and outputs — as per-stage spans in the run journal, rolled up into the goober-run
  telemetry store (`GBO-020`). *(All tiers)*
- **TSK-031 (MUST):** Task failure or timeout MUST be surfaced to the runner for
  retry/recovery (mechanism owned by Workflow/Scheduler). *(All tiers)*
- **TSK-032 (SHOULD):** A Task SHOULD be idempotent or safely retryable, given retries
  are a first-class runner capability (`WF-021`).

### Stage contract (run environment, artifacts, capabilities)
- **TSK-040 (MUST):** Every Task MUST execute in an ephemeral, isolated, disposable
  workspace. Repo-backed Tasks MUST receive a **fresh working copy** of the target repo
  — at tiers 1–2 a git worktree off the managed working copy, run as a local process;
  at tier 3 (V2) the workspace of an ephemeral pod. Deterministic Tasks MAY instead
  declare `run.workspace: scratch` to receive an empty workspace without repository
  resolution. Environments MUST be cleaned up after the run. This is the **owning
  statement** of the stage run-environment contract (`ARCHITECTURE.md §5`; `WF-053`
  and `GBO-050` defer here). *(All tiers)*
- **TSK-041 (MUST):** Tasks MUST exchange data only via envelopes and **artifact
  pointers** (path + content digest inside the run journal); a Task MUST NOT reach
  into another Task's state or rely on implicit shared state. *(All tiers)*
- **TSK-042 (MUST):** A Task MUST only exercise capabilities its definition declares
  (e.g. `github:issues:write`, `repo:push`, `telemetry:read`); undeclared use MUST
  fail closed (capability admission). Owning requirement: `SEC-042`, which names the
  per-tier enforcing components; this ID defers to it. *(All tiers)*
- **TSK-043 (MUST):** A Task MUST declare its execution policy — timeout and retry
  policy for all tasks; command and environment additionally for deterministic tasks.
  Retries are driven by this declared policy, and a retried Task MUST appear in the
  run journal as a new attempt, never as overwritten history. *(All tiers)*
- **TSK-044 (MUST):** The task runner MUST scrub secret material before any event,
  span, or artifact from a Task is written to the run journal (redaction at the
  boundary). Owning requirement: `SEC-041` (registry + pattern scanning,
  scrub-before-digest, sanctioned remediation); this ID defers to it. *(All tiers)*

## Relationships

- Part of → a **Workflow** (it is a state in the compiled machine, advanced by the
  local runner at tiers 1–2 and the Temporal runner at tier 3).
- Agentic tasks invoke → a **Goober** (via its harness adapter).
- Followed/guarded by → **Gates** (which validate result envelopes and branch).
- Emits → events, spans, and artifacts into the **run journal**, rolled up into the
  **Telemetry** store.

## Open questions

- **TSK-Q1:** ~~Common context schema~~ **Resolved (shape, updated 2026-07-12):** a
  standard JSON **invocation envelope** — goal, context pointers, capability grants —
  carrying `taskId, workflowId, runId, gaggle, item/trigger payload, goal, repoRef,
  upstreamOutputs (as artifact pointers), limits` + a task-specific `inputs` blob
  (`ARCHITECTURE.md §5`). *(Remaining: finalize fields.)*
- **TSK-Q2:** ~~Standard result contract~~ **Resolved (shape, updated 2026-07-12):** a
  standard **result envelope** — `status (success|failed|needs-escalation), outputs,
  artifacts (artifact pointers: path + digest in the run journal; e.g. PR links),
  summary, metrics, error?`. Gates, the journal, and telemetry depend on this shape
  (`ARCHITECTURE.md §5`). *(Remaining: finalize fields.)*
- **TSK-Q3:** **Resolved:** parallelism is expressed at the **workflow** level (a task =
  one goober run), not by fanning out within a single task.
