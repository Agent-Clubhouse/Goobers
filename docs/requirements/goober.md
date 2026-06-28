# Spec: Goober

> Status: **Draft** · Derives from `../VISION.md` §5, §6, §7 · Area prefix: `GBO`

## Purpose

A **Goober** is a role-specialized AI worker, defined as code, that a workflow invokes
to perform agentic tasks. It is the labor unit of the platform.

## Model

- A Goober is fundamentally a **definition** living in the `config` repo (config-as-
  code). The dashboard "team member" is this definition.
- At runtime a Goober **materializes as ephemeral pod(s)** when a workflow invokes it for
  an agentic task. Pods are transient; durable state lives outside (project repo,
  backlog, goober-run telemetry store).
- A Goober **never orchestrates or schedules** itself. It is always invoked *by a
  workflow* (the floor being a simple single-stage workflow — there is no "outside a
  workflow"). The system scheduler decides when; the workflow decides what.
- A Goober is **harness-backed**: v1 = the GitHub Copilot agent harness, running standard
  tools (MCPs, skills, instruction markdown).

## Composition of a Goober definition

| Element | Description |
|---|---|
| Identity | Name + role (e.g. "coder", "perf-hunter"). |
| Instructions | Instruction markdown defining behavior/persona/scope. |
| Skills | Skill set available to the goober. |
| Tools | MCP servers / tools it can use (incl. management tools — see telemetry). |
| Harness binding | The agent harness it runs on (v1: Copilot). |
| Scale factor | Desired replica count for concurrent work. |
| Workflow association | Which workflow(s) invoke this goober (and for which tasks). |

## Invocation contract (runtime)

1. A workflow task targets a goober. The system prepares a pod: auth, harness, signed
   in, **fresh copy of the target repo**.
2. An **invocation hook** hands the goober a block of **context/data + the task
   definition** (inputs, work to do, goal).
3. The goober performs the work.
4. The goober **calls a specific completion tool/method** to signal completion + result,
   so the workflow/system can track state.
5. **Management tools (MCP/hooks) auto-collect telemetry**; machine logs are collected.

## Requirements

### Definition & config-as-code
- **GBO-001 (MUST):** A Goober MUST be fully defined as code in the `config` repo
  (markdown + folders + YAML manifest); no UI is required to create or modify one.
- **GBO-002 (MUST):** A Goober definition MUST declare: identity/role, instructions,
  skills, tools, harness binding, scale factor, and workflow association.
- **GBO-003 (MUST):** Deploying a new/updated Goober definition MUST cause it to appear
  (or update) on the portal dashboard as a team member.
- **GBO-004 (SHOULD):** Goober definitions SHOULD be composable/reusable (shared
  instruction or skill fragments) to avoid duplication across similar goobers.

### Runtime & invocation
- **GBO-010 (MUST):** A Goober MUST only execute work when invoked by a workflow task;
  it MUST NOT self-schedule.
- **GBO-011 (MUST):** Each run MUST be an ephemeral, isolated environment prepared with
  auth, the harness (signed in), and a fresh copy of the target repo.
- **GBO-012 (MUST):** On invocation a Goober MUST receive a context/data block + a task
  definition (inputs, work, goal) via the invocation hook.
- **GBO-013 (MUST):** A Goober MUST signal completion (and result/status) by calling the
  designated completion tool/method so the workflow can advance.
- **GBO-014 (MUST):** A run that fails or never signals completion MUST be observable to
  the workflow engine for retry/recovery (mechanism owned by the Workflow/Scheduler
  specs).
- **GBO-015 (SHOULD):** A Goober SHOULD have no durable local state; all persistence
  goes to systems of record (repos, backlog, telemetry store).

### Telemetry
- **GBO-020 (MUST):** Every Goober run MUST emit telemetry to the goober-run telemetry
  store: start/stop, per-step logs, agent actions, and script/tool outputs.
- **GBO-021 (MUST):** Telemetry collection MUST be automatic (injected management
  tools/hooks + machine log collection), not dependent on the goober opting in.

### Scaling
- **GBO-030 (MUST):** A Goober's concurrency MUST be controlled by a scale factor in its
  definition; increasing it + redeploy yields more concurrent replicas.
- **GBO-031 (MUST):** Scaled replicas MUST claim work so no two replicas process the same
  item (mechanism owned by the Scheduler spec).

### Harness
- **GBO-040 (MUST):** v1 MUST support the GitHub Copilot agent harness.
- **GBO-041 (WON'T v1):** A pluggable multi-harness abstraction is out of scope for v1
  (Claude Code support deferred).

## Relationships

- Invoked by → **Workflow** (Task). A Goober may be referenced by multiple workflows.
- Belongs to → **Gaggle** (siloed). 
- Produces → changes/PRs to the project repo and/or items in the backlog.
- Emits → records into the goober-run **Telemetry** store (consumed by the **Tutor**).
- Modified by → the **Tutor**, via PRs against this definition.

## Open questions

- **GBO-Q1:** **Resolved:** exactly-once via Temporal workflow identity (`SCH-020`).
- **GBO-Q2:** **Resolved (default):** per-goober tool **allowlist** (default-deny),
  see `SEC-Q4`.
- **GBO-Q3:** ~~Standard shape of the context/data block~~ **Resolved (shape):** the
  standard JSON invocation envelope + task-specific `inputs` (see `TSK-Q1`). *(Remaining:
  finalize fields.)*
- **GBO-Q4:** **Resolved:** a goober definition MAY be referenced by **multiple
  workflows**; per-task invocation envelope differentiates behavior.
