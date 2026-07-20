# Spec: Workflow

> Status: **Draft** · Aligned to ../ARCHITECTURE.md (2026-07-12) · Derives from ../VISION.md §5, §6, §7 · Area prefix: WF

## Purpose

A **Workflow** is a defined process — modeled as a deterministic state machine — that
the system scheduler invokes to get work done. It is the unit of orchestration:
everything a goober does happens inside a workflow, at every deployment tier.

## Model

- A Workflow is **defined as code** within a gaggle and composed of **Tasks** (states:
  deterministic or agentic) and **Gates** (validation/branching states).
- A workflow definition **compiles to a deterministic step-machine**. All side effects
  live inside stages; the machine itself reads no wall clock and takes no hidden
  inputs. The compiled machine is advanced by a **runner** behind a single seam
  (`ARCHITECTURE.md §3`): the **local runner** (tiers 1–2, ships first) and the
  **Temporal runner** (tier 3, V2). Same definition + pinned inputs ⇒ semantically
  identical **run journals** on either runner.
- Every run produces an **append-only run journal** (`ARCHITECTURE.md §4`) — pinned
  identity, state checkpoint, event journal, immutable input snapshots,
  content-digested artifacts, per-stage spans. The journal — not any runner's
  internals — is the product's history.
- The **system scheduler** decides when to start a run. A workflow is eligible for a
  new run **IFF** its **trigger** fires *and* its **readiness conditions** are met.
- **Triggers** differentiate workflow archetypes but not the taxonomy: a schedule
  (cron / time-since-last-run) — first to ship (V0); a backlog item becoming
  available; or an external signal. Perf hunter, error miner, Tutor, researchers,
  implementers are **all just workflows** differing by trigger + stages. At V0,
  backlog consumption is expressed as a cron-triggered workflow whose first stage
  queries the provider for eligible items and claims them.
- The **default starter** is an ordinary **length-1 (single-stage), implement-only**
  workflow, shipped so a gaggle works immediately.
- Every run is a **trace**: journal spans plus the goober-run telemetry store.

## Requirements

### Definition & structure
- **WF-001 (MUST):** A Workflow MUST be fully defined as code in the `config`
  repo/directory (config-as-code); no UI is required to create or modify one.
  *(All tiers)*
- **WF-002 (MUST):** A Workflow MUST be modeled as a state machine composed of Tasks
  and Gates, and MUST compile to a **deterministic step-machine** — no side effects,
  wall-clock reads, or hidden inputs in the machine itself. *(All tiers)*
- **WF-003 (MUST):** A Workflow MUST belong to exactly one Gaggle.
- **WF-004 (MUST):** The platform MUST ship a default single-stage, implement-only
  workflow so a newly created gaggle can do work without authoring a workflow first.
- **WF-005 (SHOULD):** Workflows SHOULD support composition/reuse of common stages or
  fragments to avoid duplication.

### Triggers, readiness & scheduling
- **WF-010 (MUST):** A Workflow MUST declare its trigger(s): schedule/cron/
  time-since-last-run (**ships first, V0**), backlog-item-available, and/or external
  signal. Direct backlog-item triggers and their selectors remain in the schema but
  are **reserved for V1** and have no V0 runtime consumer. At V0 backlog consumption
  is expressed as a **cron-triggered workflow whose first stage queries and claims
  eligible items** (see `WF-055`, `SCH-041`).
- **WF-011 (MUST):** A Workflow MUST declare readiness conditions (e.g. max parallel
  runs, run budgets, worker/resource capacity). A run MUST start only when the
  trigger has fired AND readiness conditions are satisfied. *(All tiers)*
- **WF-012 (MUST):** The system scheduler MUST own the decision to start runs; a
  workflow MUST NOT start itself outside the scheduler. (See Scheduler spec.)
- **WF-013 (MUST):** Producer (schedule/signal-triggered) and consumer
  (backlog-triggered) workflows MUST use the same taxonomy — no separate "producer"
  concept.
- **WF-014 (SHOULD):** A workflow's outputs (e.g. a filed backlog item, an emitted
  signal) SHOULD be able to trigger another workflow — always routed through the
  scheduler, never via direct invocation.
- **WF-015 (MUST):** Emergent chains MUST be bounded to prevent runaway loops — the
  scheduler MUST enforce per-workflow run budgets/rate limits plus chain-depth / loop
  detection.
- **WF-016 (MUST):** Workflow definition changes MUST use version pinning — a
  **runner-seam contract requirement, implemented by both runners**
  (`ARCHITECTURE.md §4`), not a feature of any particular engine: each run records
  the definition version it started on (`run.yaml`) and completes on that version;
  definition changes affect only new runs (no mid-flight mutation). *(All tiers)*

### Execution semantics
- **WF-020 (MUST):** The engine contract is the **deterministic step-machine over the
  run journal** (`ARCHITECTURE.md §3–§4`). The **local runner** MUST implement it
  first (tiers 1–2, V0); **Tier 3 (V2):** the **Temporal runner** hosts the same
  compiled machine as a Temporal workflow and projects history into the same journal
  format. Runner internals are never the product surface.
- **WF-021 (MUST):** The runner MUST support per-step data collection, retries
  (driven by each stage's declared policy; retried attempts appear in the journal as
  new attempts, never overwritten history), and crash recovery. *(All tiers)*
- **WF-022 (MUST):** Each run MUST produce a trace recorded to the goober-run
  telemetry store (see Telemetry spec) — journal spans + local rollup at tiers 1–2;
  **Tier 3 (V2):** OTLP → ADX.
- **WF-023 (MUST):** A workflow MUST support both deterministic (code-driven) Tasks
  and agentic (goober-executed) Tasks. (See Task spec.)
- **WF-024 (MUST):** Gates MUST be able to conditionally branch the flow. (See Gate
  spec.)

### Concurrency & scaling
- **WF-030 (MUST):** The platform MUST support multiple concurrent runs of a
  workflow, bounded by its readiness/concurrency limits. *(All tiers)*
- **WF-031 (MUST):** Concurrent/scaled execution MUST use a work-claiming mechanism
  so no unit of work is processed more than once. (See Scheduler spec.)

### Routing
- **WF-040 (MUST, V1):** The platform MUST map an incoming unit of work (e.g. a
  backlog item) to the correct workflow. Mechanism owned by the Scheduler spec
  (labels + selectors). The V0 schema reserves these fields but does not consume
  them at runtime.

### Run journal & runner-seam contract
- **WF-050 (MUST):** Every run MUST produce an append-only, content-digested **run
  journal** (`ARCHITECTURE.md §4`): pinned identity (`run.yaml`), atomically
  replaced state checkpoint, append-only event journal, immutable input snapshots,
  digest-addressed artifacts, and per-stage spans. Nothing in a journal is edited
  after the fact; repairs append corrective events (secret remediation per
  `SEC-041` is the one sanctioned exception). *(All tiers)*
- **WF-051 (MUST):** Both runners MUST implement the same runner-seam contract: the
  same workflow definition with **fixed stage effects** MUST produce **equivalent
  run journals** on either runner, where equivalence is the defined conformance
  relation of `ARCHITECTURE.md §3.3` — the ordered orchestration-event set compared
  after canonicalization; timestamps/durations, infrastructure-retry attempts, and
  namespaced `runner.*` annotations are excluded. For live agentic runs the
  guarantee is structural (same machine, same branching for the same verdicts),
  never payload equality. **Tier 3 (V2):** a conformance harness runs shared
  fixtures through both runners and diffs the conformance set.
- **WF-052 (MUST):** Stages MUST exchange **invocation/result envelopes** and
  **artifact pointers** (path + digest inside the journal) — never implicit shared
  state; no stage reaches into another stage's state. Owning contract: `TSK-041`
  (`ARCHITECTURE.md §5`); this ID defers to it. *(All tiers)*
- **WF-053 (MUST):** Each stage MUST execute in a **fresh, isolated, disposable
  workspace**, torn down after the run. Repo-backed stages receive a working copy of
  the target repo; deterministic stages MAY instead request an empty scratch workspace.
  Owning contract: `TSK-040` (`ARCHITECTURE.md §5`); this ID defers to it. *(Tiers
  1–2: a git worktree or local scratch directory, run as a local process; Tier 3
  (V2): the workspace of an ephemeral Kubernetes agent pod.)*
- **WF-054 (MUST):** The local runner MUST recover from a crash by replaying the
  state checkpoint + event journal on restart and resuming from the last completed
  stage — durability is append + fsync, with no database or service dependency.
  *(Tiers 1–2)*
- **WF-055 (MUST):** V0 MUST support expressing backlog consumption as a
  cron-triggered workflow whose first stage — the built-in **`backlog-query`** stage
  kind — queries the provider for eligible items and **claims** them so concurrent
  runs never double-process. Owning requirement: `SCH-041` (claiming semantics
  `SCH-020`; eligibility gate `SEC-047`); this ID defers to it. *(All tiers; ships
  tiers 1–2 first.)*

## Relationships

- Invoked by → the **system Scheduler** (on trigger + readiness).
- Advanced by → a **Runner** behind the runner seam (local runner at tiers 1–2;
  Temporal runner at tier 3).
- Composed of → **Tasks** and **Gates**.
- Invokes → **Goobers** (for agentic tasks).
- Belongs to → a **Gaggle**.
- Emits → a **run journal** per run, projected into the **Telemetry** store.

## Open questions

- **WF-Q1:** ~~Routing model~~ **Resolved:** labels + selectors, priority → single
  winner (`SCH-010`/`SCH-011`). Unchanged by tier.
- **WF-Q2:** ~~Chain expression/bounding~~ **Resolved:** emergent via scheduler
  triggers (`WF-014`); bounded by run budgets + chain-depth/loop detection (`WF-015`).
- **WF-Q3:** ~~Mid-flight definition changes~~ **Resolved:** version pinning as a
  runner-seam contract requirement, implemented by both runners (`WF-016`,
  `ARCHITECTURE.md §4`).
- **WF-Q4:** ~~Confirm engine choice~~ **Resolved (superseding the earlier
  "Temporal, self-hosted" resolution):** the engine contract is the **deterministic
  step-machine over the run journal**; the **local runner** implements it first
  (V0), and **Temporal** is the tier-3 drop-in behind the same seam (V2), projecting
  history into the same journal format. See `ARCHITECTURE.md §3` and
  `deployment.md DEP-011`.
