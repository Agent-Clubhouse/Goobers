# Spec: Scheduler & Work Distribution

> Status: **Draft** · Aligned to ../ARCHITECTURE.md (2026-07-12) · Derives from ../VISION.md §7 · Area prefix: SCH

## Purpose

The **Scheduler** is the deterministic system component that decides *when* workflow runs
start, *which* workflow a unit of work goes to (**routing**), and ensures each unit is
processed *exactly once* across scaled replicas (**claiming**). It is the orchestrator
referenced throughout `VISION.md §7` — goobers and workflows never self-start outside
it. Its
**declared semantics are identical at every tier**; only the substrate underneath
changes (`ARCHITECTURE.md §7`).

## Model

- **Embedded at tiers 1–2:** the scheduler is **embedded in the local runner daemon**
  (`goobers up`) — no separate scheduler service. It evaluates cron-expression
  triggers and enforces run conditions: max parallel runs per workflow/instance and
  per-workflow run budgets.
- **Tier 3 (V2):** the same declared schedule semantics map onto **Temporal
  Schedules**, and claiming coordinates across distributed workers via workflow-id
  identity — same semantics, different substrate.
- **Admission loop:** the scheduler continuously evaluates each workflow — has its
  **trigger** fired and are its **readiness conditions** met (`WF-010`/`WF-011`)? If yes
  and matching work exists, it starts a run.
- **Routing — labels + selectors:** units of work (e.g. backlog items) carry
  **labels**; workflows declare **selectors** over those labels. **Shipped:** the
  backlog-item trigger consumes selector KEYS as required GitHub labels when
  counting eligible items (values are ignored — GitHub labels are flat strings).
  **V1 prescriptive:** full k8s-style selector matching and multi-workflow
  routing (`SCH-010`/`SCH-011`).
- **Claiming — lease-based atomic claim, owned by the runner:** before a run, the
  scheduler claims the unit atomically. At tiers 1–2 the claim is recorded in a
  **claim ledger in instance state** plus a **provider-visible marker**
  (label/assignee) on the item; on failure/timeout/crash the lease **auto-releases**
  for retry. At tier 3 the same guarantee comes from Temporal workflow identity.
- **Indirect triggering:** a goober/workflow output (filed item, signal) is admitted as
  a trigger *through the scheduler* — never via direct invocation (`WF-014`).

## Requirements

### Admission & readiness
- **SCH-001 (MUST):** The scheduler MUST own all decisions to start workflow runs; its
  behavior MUST be deterministic. *(All tiers)*
- **SCH-002 (MUST):** A run MUST start only when the workflow's trigger has fired AND
  its readiness conditions are satisfied.
- **SCH-003 (MUST):** The scheduler MUST enforce concurrency limits / capacity before
  starting runs (no oversubscription) — at minimum max-parallel per workflow/instance
  and per-workflow run budgets.
- **SCH-004 (SHOULD):** The scheduler SHOULD apply backpressure gracefully when capacity
  is saturated (queue/defer rather than fail).

### Routing
- **SCH-010 (MUST, V1 full form):** The scheduler MUST route a unit of work to
  workflow(s) by matching item **labels** against workflow **selectors**.
  **Shipped:** the backlog-item trigger filters on selector KEYS as required
  GitHub labels when counting eligible items (values ignored — GitHub labels are
  flat strings). **Not implemented — V1 prescriptive:** true selector semantics
  (value matching, set expressions) and routing one item across multiple
  candidate workflows.
- **SCH-011 (MUST):** When an item matches **multiple** workflows, the scheduler MUST
  resolve to a **single** winner by declared **priority** (deterministic tiebreak) —
  preserving one-item-one-workflow so exactly-once claiming holds on either runner.
  *(Not implemented — V1 prescriptive.)*
- **SCH-012 (MUST):** When an item matches **no** workflow, it MUST go to a visible
  **dead-letter / unrouted** state for human attention. A catch-all **default workflow**
  MAY be configured per gaggle to handle unmatched items instead.
  *(Not implemented — V1 prescriptive.)*

### Claiming & exactly-once
- **SCH-020 (MUST):** Exactly-once processing MUST be enforced **instance-side by the
  runner** (`WF-031`, `GBO-031`). *(Tiers 1–2)*: a **lease-based atomic claim**
  recorded in a **claim ledger in instance state**, mirrored by a
  **provider-visible marker** (label/assignee) on the item, so no two runs process
  the same item. **Tier 3 (V2):** the scheduler starts one Temporal workflow per unit
  of work using a deterministic id derived from the item; Temporal rejects duplicate
  ids — same exactly-once guarantee, different substrate.
- **SCH-021 (MUST):** Crash/failure recovery MUST NOT lose or orphan claims.
  *(Tiers 1–2)*: leases carry a visibility timeout and **auto-release** on
  failure/timeout/crash; the local runner recovers in-flight runs by journal replay
  (`WF-054`). **Tier 3 (V2):** Temporal's durable execution recovers and retries the
  workflow. On terminal failure the scheduler MAY re-admit the item.
- **SCH-022b (SHOULD):** The backlog item SHOULD be updated to mirror processing status
  (claimed/in-progress/done) for human visibility — this is a reflection, not the
  source of claim truth.
- **SCH-022 (SHOULD):** Claiming SHOULD be visible/observable (which item is claimed by
  which run) for debugging and the portal — the claim ledger and journal events make
  this inspectable with standard tools at tier 1.

### Prioritization & telemetry
- **SCH-030 (SHOULD):** The scheduler SHOULD support backlog prioritization (which item
  next) — explicit priority field, FIFO within a priority (see `SCH-Q3`).
  *(Not implemented — V1 prescriptive; no priority field exists yet.)*
- **SCH-031 (MUST):** The scheduler MUST emit telemetry for its decisions, claims, and
  releases to the goober-run telemetry store — at tiers 1–2 as events in the
  **instance journal** (`scheduler/events.jsonl`, `ARCHITECTURE.md §4/§6`) rolled up
  locally; ADX at tier 3.

### Embedding & triggers by tier
- **SCH-040 (MUST):** At tiers 1–2 the scheduler MUST be embedded in the local runner
  daemon (`goobers up`) — a single binary with no database, message bus, or separate
  scheduler service. *(Tiers 1–2)*
- **SCH-041 (MUST):** Cron-expression triggers MUST ship first (V0): the embedded
  scheduler evaluates them and enforces run conditions (max-parallel, run budgets).
  **Shipped since:** all five trigger types (`manual`, `schedule`, `backlog-item`,
  `signal`, `webhook`) now have live runtime consumers. The backlog-item trigger
  fans out on the provider's eligible-item count (#344), filtered by selector
  KEYS as required labels; the run's first stage — the built-in **`backlog-query`**
  deterministic stage kind — still performs the query + claim, honoring the
  untrusted-input eligibility gate (`SEC-047`). This is the owning statement of
  the query-and-claim pattern (`WF-055` defers here; `WF-010`). Full selector
  matching and multi-workflow routing remain V1 (`SCH-010`–`SCH-012`). *(All
  tiers; ships tiers 1–2 first)*
- **SCH-042 (MUST):** **Tier 3 (V2):** declared schedule triggers MUST map onto
  **Temporal Schedules**, and claiming MUST use workflow-id-based exactly-once
  identity (`SCH-020`) — the cloud drop-in for the scheduling seam
  (`ARCHITECTURE.md §7, §10`). Declared semantics MUST NOT change across the seam.

## Relationships

- Embedded in → the **local runner daemon** (tiers 1–2); maps onto **Temporal
  Schedules** (tier 3, V2).
- Starts → **Workflow** runs (on trigger + readiness).
- Reads/claims from → the **Backlog** (and other trigger sources), via the claim
  ledger + provider-visible marker.
- Enforces → concurrency for **Goober**/**Workflow** scaling.
- Emits → scheduling telemetry into the **Telemetry** store.

## Open questions

- **SCH-Q1:** ~~Multi-match routing policy~~ **Resolved:** priority → single winner
  (`SCH-011`).
- **SCH-Q2:** ~~Unrouted-item handling~~ **Resolved:** dead-letter + visible; optional
  catch-all default workflow (`SCH-012`).
- **SCH-Q3:** **Resolved (default):** explicit **priority field** on workflows, FIFO
  within a priority. Aging/pluggable policies are a future extension.
- **SCH-Q4:** ~~Engine choice~~ **Resolved (superseding the earlier "engine =
  Temporal" resolution):** the scheduler is **built, not bought** — embedded in the
  local runner at tiers 1–2; at tier 3 the same declared semantics drop onto
  Temporal Schedules behind the runner seam. See `ARCHITECTURE.md §3, §7`.
  *(Build-time: which Temporal primitives the tier-3 mapping leans on.)*
- **SCH-Q5:** ~~Where leases/claims are stored~~ **Resolved (updated):**
  instance-side, owned by the runner — claim ledger in instance state +
  provider-visible marker at tiers 1–2; Temporal workflow identity at tier 3. The
  backlog item mirrors status only. See `SCH-020`/`SCH-021`, `ARCHITECTURE.md §7`.
