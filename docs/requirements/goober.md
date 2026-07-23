# Spec: Goober

> Status: **Draft** · Aligned to `../ARCHITECTURE.md` (2026-07-12) · Derives from
> `../VISION.md` §5, §6, §7 · Area prefix: `GBO`

## Purpose

A **Goober** is a role-specialized AI worker, defined as code, that a workflow invokes
to perform agentic tasks. It is the labor unit of the platform — identical in shape at
every deployment tier: the same definition runs on a solo laptop, a team box, or a
cloud cluster without change.

## Model

- A Goober is fundamentally a **definition** living in the `config` repo/directory
  (config-as-code). The roster "team member" is this definition.
- At runtime a Goober **materializes as ephemeral run environments** when a workflow
  invokes it for an agentic task: an isolated **git worktree + local process** at
  tiers 1–2 (local runner), an **ephemeral Kubernetes pod** at tier 3 (Temporal
  runner, V2). Run environments are transient; durable state lives outside (project
  repo, backlog, run journal, goober-run telemetry store).
- A Goober **never orchestrates or schedules** itself. It is always invoked *by a
  workflow* (the floor being a simple single-stage workflow — there is no "outside a
  workflow"). The system scheduler decides when; the workflow decides what.
- A Goober is **harness-backed**: the first harness adapter is the **GitHub Copilot
  CLI**, running standard tools (MCPs, skills, instruction markdown). Harness choice
  is a **stage-level adapter detail** behind one invocation/result envelope contract
  (`ARCHITECTURE.md §5`), never an architectural commitment.
- Everything a Goober run does is recorded in the run's **run journal** — append-only
  events, per-stage spans (including within-stage harness events), and
  content-digested artifacts referenced by pointer.

## Composition of a Goober definition

| Element | Description |
|---|---|
| Identity | Name + role (e.g. "coder", "perf-hunter"). |
| Instructions | Instruction markdown defining behavior/persona/scope. |
| Assets | Optional static files under `assets/`, available to every invocation at workspace path `.goober-assets/`. |
| Skills | Skill set available to the goober. |
| Tools | MCP servers / tools it can use (incl. management tools — see telemetry), declared as a capability allowlist. |
| Harness binding | The harness adapter it runs on (first adapter: GitHub Copilot CLI). |
| Scale factor | Desired replica count for concurrent work. |
| Workflow association | Which workflow(s) invoke this goober (and for which tasks). |

## Invocation contract (runtime)

1. A workflow task targets a goober. The runner prepares an ephemeral run environment
   — auth, harness signed in, a **fresh, isolated working copy** of the target repo
   (a git worktree + local process at tiers 1–2; an ephemeral pod at tier 3).
2. An **invocation envelope** hands the goober the goal, context pointers, capability
   grants, and the task definition (inputs, work to do, goal).
3. The goober performs the work.
4. The goober **finishes by producing a result envelope** (status, outputs, artifact
   pointers), so the workflow/runner can track completion and advance.
5. **Management tools (MCP/hooks) auto-collect telemetry**; harness events and machine
   logs land as spans in the run journal.

## Requirements

### Definition & config-as-code
- **GBO-001 (MUST):** A Goober MUST be fully defined as code in the `config`
  repo/directory (markdown + folders + YAML manifest); no UI is required to create or
  modify one. *(All tiers)*
- **GBO-002 (MUST):** A Goober definition MUST declare: identity/role, instructions,
  skills, tools (capability allowlist), harness binding, scale factor, and workflow
  association. *(All tiers)*
- **GBO-003 (MUST):** Loading a new/updated Goober definition (local `config/` pickup
  at tiers 1–2; GitOps sync at tier 3) MUST cause it to appear (or update) on the
  portal roster as a team member. *(All tiers)*
- **GBO-004 (SHOULD):** Goober definitions SHOULD be composable/reusable (shared
  instruction or skill fragments) to avoid duplication across similar goobers.
- **GBO-005 (MUST):** When a Goober definition includes an `assets/` directory,
  each invocation MUST receive an isolated snapshot of that directory at the
  fixed workspace-relative path `.goober-assets/`. Goobers without `assets/`
  MUST be unaffected, and assets MUST NOT be shared across Goobers or stages.
  The reserved workspace path MUST NOT be tracked on a run branch. *(All tiers)*

### Runtime & invocation
- **GBO-010 (MUST):** A Goober MUST only execute work when invoked by a workflow task;
  it MUST NOT self-schedule. *(All tiers)*
- **GBO-011 (MUST):** Each run MUST be an ephemeral, isolated run environment — an
  isolated git worktree + local process at tiers 1–2, an ephemeral pod at tier 3 —
  prepared with auth, the harness (signed in), and a fresh working copy of the target
  repo. *(All tiers)*
- **GBO-012 (MUST):** On invocation a Goober MUST receive an **invocation envelope**
  (goal, context pointers, capability grants) plus the task definition (inputs, work,
  goal). *(All tiers)*
- **GBO-013 (MUST):** A Goober MUST signal completion by producing the standard
  **result envelope** (status, outputs, artifact pointers) so the workflow can
  advance. *(All tiers)*
- **GBO-014 (MUST):** A run that fails or never produces a result envelope MUST be
  observable to the runner for retry/recovery; a retried run appears in the run
  journal as a new attempt, never as overwritten history (mechanism owned by the
  Workflow/Scheduler specs). *(All tiers)*
- **GBO-015 (SHOULD):** A Goober SHOULD have no durable local state; all persistence
  goes to systems of record (repos, backlog) and the run journal/telemetry store.

### Telemetry
- **GBO-020 (MUST):** Every Goober run MUST emit telemetry — start/stop, per-step
  logs, agent actions, and script/tool outputs — as spans in the run journal and into
  the goober-run telemetry store (journal spans + local rollup at tiers 1–2; ADX via
  OTLP at tier 3). *(All tiers)*
- **GBO-021 (MUST):** Telemetry collection MUST be automatic (injected management
  tools/hooks + machine log collection), not dependent on the goober opting in.
  *(All tiers)*

### Scaling
- **GBO-030 (MUST):** A Goober's concurrency MUST be controlled by a scale factor in
  its definition; increasing it and reloading/redeploying the definition yields more
  concurrent replicas. *(All tiers)* **Status:** `scaleFactor` is schema-reserved
  but the local runner does not consume it — local concurrency is governed by
  per-workflow `maxConcurrentRuns` readiness conditions. Only the quarantined
  tier-3 operator (`internal/operator`) consumes it today; local-runner
  consumption (or supersession by workflow-level limits) is a pending V1/V2
  decision.
- **GBO-031 (MUST):** Scaled replicas MUST claim work so no two replicas process the
  same item — a lease-based atomic claim owned by the runner (mechanism owned by the
  Scheduler spec). *(All tiers)*

### Harness
- **GBO-040 (MUST):** The first harness adapter MUST be the **GitHub Copilot CLI**;
  V0/V1 ship with it as the supported harness. *(All tiers)*
- **GBO-041 (WON'T (v1)):** Additional harness adapters (e.g. Claude Code) are out of
  scope for v1. The invocation/result envelope contract (`GBO-051`) already *is* the
  harness seam — adding a harness later means writing an adapter behind it, not
  building a new abstraction. *(Supersedes this ID's earlier, broader exclusion of
  any multi-harness abstraction: the seam is now core architecture (`GBO-051`); only
  additional adapters are deferred. Until sandboxing lands (`SEC-044`, V1), each
  adapter carries the capability-enforcement burden — it materializes only granted
  credentials/tools into the harness session — so new adapters are security-critical
  code, not plug-ins to accept casually.)*

### Run environment & journal
- **GBO-050 (MUST):** At tiers 1–2 a Goober run environment MUST be a local process
  executing in an isolated git worktree branched from the instance's managed working
  copy of the target repo; worktrees are disposable and MUST be cleaned up after the
  run. Owning contract: `TSK-040`; this ID defers to it. *(Tiers 1–2)*
- **GBO-051 (MUST):** A harness MUST be driven exclusively through the standard
  invocation envelope and MUST terminate by producing the standard result envelope;
  the harness binding is a per-goober adapter detail and MUST NOT leak into workflow,
  gate, or journal contracts. *(All tiers)*
- **GBO-052 (MUST):** A Goober MUST only exercise capabilities declared in its
  definition and granted by the invoking task's envelope (e.g. `github:issues:write`,
  `repo:push`); undeclared capability use MUST fail closed. Owning requirement:
  `SEC-042`, which names the per-tier enforcing components; this ID defers to it.
  *(All tiers)*
- **GBO-053 (MUST):** Credentials and secrets available to a Goober run MUST be
  scrubbed before any event, span, or artifact is written to the run journal
  (redaction at the boundary). Owning requirement: `SEC-041`; this ID defers to it.
  *(All tiers)*

## Relationships

- Invoked by → **Workflow** (Task). A Goober may be referenced by multiple workflows.
- Belongs to → **Gaggle** (siloed).
- Produces → changes/PRs to the project repo and/or items in the backlog.
- Emits → events/spans into the **run journal** and the goober-run **Telemetry** store
  (consumed by the **Tutor**).
- Modified by → the **Tutor**, via PRs against this definition.

## Open questions

- **GBO-Q1:** **Resolved (updated 2026-07-12):** exactly-once work processing is a
  **lease-based atomic claim owned by the runner** — a claim ledger in instance state
  plus a provider-visible marker at tiers 1–2; Temporal workflow-id identity at
  tier 3 (`ARCHITECTURE.md §7`, `SCH-020`). *(Previously resolved as Temporal-only;
  the runner seam generalizes it.)*
- **GBO-Q2:** **Resolved (default):** per-goober tool **allowlist** (default-deny),
  see `SEC-Q4`; enforced via capability admission (`GBO-052`).
- **GBO-Q3:** ~~Standard shape of the context/data block~~ **Resolved (shape):** the
  standard JSON **invocation envelope** — goal, context pointers, capability grants —
  plus task-specific `inputs` (see `TSK-Q1`, `ARCHITECTURE.md §5`). *(Remaining:
  finalize fields.)*
- **GBO-Q4:** **Resolved:** a goober definition MAY be referenced by **multiple
  workflows**; the per-task invocation envelope differentiates behavior.
