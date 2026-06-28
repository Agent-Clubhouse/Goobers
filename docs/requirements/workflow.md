# Spec: Workflow

> Status: **Draft** · Derives from `../VISION.md` §5, §6, §7 · Area prefix: `WF`

## Purpose

A **Workflow** is a defined process — modeled as a deterministic state machine — that
the system scheduler invokes to get work done. It is the unit of orchestration:
everything a goober does happens inside a workflow.

## Model

- A Workflow is **defined as code** within a gaggle and composed of **Tasks** (states:
  deterministic or agentic) and **Gates** (validation/branching states).
- The **system scheduler** decides when to start a run. A workflow is eligible for a new
  run **IFF** its **trigger** fires *and* its **readiness conditions** are met.
- **Triggers** differentiate workflow archetypes but not the taxonomy: a backlog item
  becoming available (consumer), a schedule / time-since-last-run, or an external signal
  (producer). Perf hunter, error miner, Tutor, researchers, implementers are **all just
  workflows** differing by trigger + stages.
- The **default starter** is an ordinary **length-1 (single-stage), implement-only**
  workflow, shipped so a gaggle works immediately.
- **Engine:** an off-the-shelf deterministic state-machine engine (Temporal is the lead
  candidate), chosen for consistency, per-step data collection, retries, and recovery.
- Every run is a **trace** captured to the goober-run telemetry store.

## Requirements

### Definition & structure
- **WF-001 (MUST):** A Workflow MUST be fully defined as code in the `config` repo
  (config-as-code); no UI is required to create or modify one.
- **WF-002 (MUST):** A Workflow MUST be modeled as a state machine composed of Tasks and
  Gates.
- **WF-003 (MUST):** A Workflow MUST belong to exactly one Gaggle.
- **WF-004 (MUST):** The platform MUST ship a default single-stage, implement-only
  workflow so a newly created gaggle can do work without authoring a workflow first.
- **WF-005 (SHOULD):** Workflows SHOULD support composition/reuse of common stages or
  fragments to avoid duplication.

### Triggers, readiness & scheduling
- **WF-010 (MUST):** A Workflow MUST declare its trigger(s): backlog-item-available,
  schedule/time-since-last-run, and/or external signal.
- **WF-011 (MUST):** A Workflow MUST declare readiness conditions (e.g. worker/resource
  capacity, concurrency limits, resource constraints). A run MUST start only when the
  trigger has fired AND readiness conditions are satisfied.
- **WF-012 (MUST):** The system scheduler MUST own the decision to start runs; a workflow
  MUST NOT start itself outside the scheduler. (See Scheduler spec.)
- **WF-013 (MUST):** Producer (schedule/signal-triggered) and consumer (backlog-triggered)
  workflows MUST use the same taxonomy — no separate "producer" concept.
- **WF-014 (SHOULD):** A workflow's outputs (e.g. a filed backlog item, an emitted
  signal) SHOULD be able to trigger another workflow — always routed through the
  scheduler, never via direct invocation.

### Execution semantics
- **WF-020 (MUST):** The workflow engine MUST be a deterministic state machine
  (off-the-shelf; Temporal is the lead candidate).
- **WF-021 (MUST):** The engine MUST support per-step data collection, retries, and
  recovery.
- **WF-022 (MUST):** Each run MUST produce a trace recorded to the goober-run telemetry
  store (see Telemetry spec).
- **WF-023 (MUST):** A workflow MUST support both deterministic (code-driven) Tasks and
  agentic (goober-executed) Tasks. (See Task spec.)
- **WF-024 (MUST):** Gates MUST be able to conditionally branch the flow. (See Gate spec.)

### Concurrency & scaling
- **WF-030 (MUST):** The platform MUST support multiple concurrent runs of a workflow,
  bounded by its readiness/concurrency limits.
- **WF-031 (MUST):** Concurrent/scaled execution MUST use a work-claiming mechanism so no
  unit of work is processed more than once. (See Scheduler spec.)

### Routing
- **WF-040 (MUST):** The platform MUST map an incoming unit of work (e.g. a backlog item)
  to the correct workflow. Mechanism owned by the Scheduler spec. *(Open — see below.)*

## Relationships

- Invoked by → the **system Scheduler** (on trigger + readiness).
- Composed of → **Tasks** and **Gates**.
- Invokes → **Goobers** (for agentic tasks).
- Belongs to → a **Gaggle**.
- Emits → a trace per run into the **Telemetry** store.

## Open questions

- **WF-Q1:** Routing model — labels/tags on backlog items, per-workflow predicates, or a
  routing table? (Scheduler.)
- **WF-Q2:** How are workflow chains expressed and bounded (avoiding runaway loops)?
- **WF-Q3:** Versioning — when a workflow definition changes mid-flight, what happens to
  in-progress runs?
- **WF-Q4:** Confirm Temporal vs. alternatives against our hosting (AKS) and harness
  constraints.
