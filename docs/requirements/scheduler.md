# Spec: Scheduler & Work Distribution

> Status: **Draft** · Derives from `../VISION.md` §7 · Area prefix: `SCH`

## Purpose

The **Scheduler** is the deterministic system component that decides *when* workflow runs
start, *which* workflow a unit of work goes to (**routing**), and ensures each unit is
processed *exactly once* across scaled replicas (**claiming**). It is the orchestrator
referenced throughout §7 — goobers and workflows never self-start outside it.

## Model

- **Admission loop:** the scheduler continuously evaluates each workflow — has its
  **trigger** fired and are its **readiness conditions** met (`WF-010`/`WF-011`)? If yes
  and matching work exists, it starts a run.
- **Routing — labels + selectors (k8s-style):** units of work (e.g. backlog items) carry
  **labels**; workflows declare **selectors** over those labels. The scheduler matches
  work to workflow(s) by selector.
- **Claiming — lease-based atomic claim:** before a run, the scheduler claims the unit via
  an **atomic lease with a visibility timeout**. Exactly one replica owns it; on
  failure/timeout/crash the lease **auto-releases** for retry.
- **Indirect triggering:** a goober/workflow output (filed item, signal) is admitted as a
  trigger *through the scheduler* — never via direct invocation (`WF-014`).

## Requirements

### Admission & readiness
- **SCH-001 (MUST):** The scheduler MUST own all decisions to start workflow runs; its
  behavior MUST be deterministic.
- **SCH-002 (MUST):** A run MUST start only when the workflow's trigger has fired AND its
  readiness conditions are satisfied.
- **SCH-003 (MUST):** The scheduler MUST enforce concurrency limits / capacity before
  starting runs (no oversubscription).
- **SCH-004 (SHOULD):** The scheduler SHOULD apply backpressure gracefully when capacity
  is saturated (queue/defer rather than fail).

### Routing
- **SCH-010 (MUST):** The scheduler MUST route a unit of work to workflow(s) by matching
  item **labels** against workflow **selectors**.
- **SCH-011 (MUST):** Behavior for an item matching **multiple** workflows MUST be defined
  (fan-out vs. priority vs. first-match). *(Open.)*
- **SCH-012 (MUST):** Behavior for an item matching **no** workflow MUST be defined
  (dead-letter / unrouted queue / default workflow). *(Open.)*

### Claiming & exactly-once
- **SCH-020 (MUST):** Before a run, a unit of work MUST be claimed via an atomic lease so
  exactly one replica processes it (`WF-031`, `GBO-031`).
- **SCH-021 (MUST):** A lease MUST carry a visibility timeout; on run failure, timeout, or
  worker crash the claim MUST auto-release for retry.
- **SCH-022 (SHOULD):** Claiming SHOULD be visible/observable (which item is claimed by
  which run) for debugging and the portal.

### Prioritization & telemetry
- **SCH-030 (SHOULD):** The scheduler SHOULD support backlog prioritization (which item
  next) — policy TBD.
- **SCH-031 (MUST):** The scheduler MUST emit telemetry for its decisions, claims, and
  releases to the goober-run telemetry store.

## Relationships

- Starts → **Workflow** runs (on trigger + readiness).
- Reads/claims from → the **Backlog** (and other trigger sources).
- Enforces → concurrency for **Goober**/**Workflow** scaling.
- May lean on → the workflow **engine** (Temporal) for parts of distribution (see opens).
- Emits → scheduling telemetry into the **Telemetry** store.

## Open questions

- **SCH-Q1:** Multi-match routing policy (`SCH-011`) — fan-out, priority, or first-match?
- **SCH-Q2:** Unrouted-item handling (`SCH-012`) — dead-letter vs. default workflow?
- **SCH-Q3:** Prioritization policy — FIFO, explicit priority field, aging, or pluggable?
- **SCH-Q4:** **Build vs. buy boundary** — how much claiming/leasing/distribution does
  Temporal provide out of the box vs. what we build (esp. backlog admission + leases on
  external backlog items)?
- **SCH-Q5:** Where leases/claims are stored (engine state vs. backlog item field vs.
  instance store) given the backlog is an *external* system of record.
