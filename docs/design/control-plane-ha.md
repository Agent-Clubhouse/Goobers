# Control plane: high availability, rolling updates and externalized state

> Status: **approved** — direction approved by the PO on 2026-10-21; per-phase details are confirmed at phase start.
> Tracking: #7694, #7695, #7696, #7697, #7698, #7699, #7700, #7701

| | |
|---|---|
| **Status** | **APPROVED in direction (PO 2026-10-21); phases P0–P6 committed; per-phase details confirmed at phase start.** Successor to [distributed-state-and-coordination](distributed-state-and-coordination.md) §10 ("Failover is deferred, deliberately"). Amends, and does not replace, the Goobernetes v1 decision record ([goobernetes-decisions](goobernetes-decisions.md)). Every departure from an approved design is listed in [§20](#departures). |
| **PO direction** | "Deliver highly distributed, fault-tolerant, scalable and updatable orchestration across Goobernetes." Disruptive changes are allowed, a hard split of the local runner from the cloud command is allowed, and per-role namespaces are wanted. The end state is committed: controller HA, per-role namespaces and engine parity are scheduled phases with entry criteria, not options. |
| **Date** | 2026-10-21 |
| **Baseline** | `main` @ `f42351fb1` (`f42351fb184be1fd1bae5dfa1ae05b9fc179e1dc`). Every `path:line` is at this commit unless stated otherwise. |
| **Related** | #2053 (fenced lease, per-process state), #3809 (single-replica daemon; HA prerequisites), #2051 (journal storage seam), #7678 (one-way read-model migrations), #7671 (terminal-report outbox), #4084 / #5407 / #5993 (worker versioning), #6447 (Temporal Schedules removed), #7590 / #7609 / #7616 (DSL 3.0 stable lock and reference migration, drafts), [engine-child-workflows](engine-child-workflows.md) (draft) |
| **Issues** | Epic and per-phase tracking issues: see the work breakdown ([§19](#work-breakdown)). |

Labels used throughout:
- **[EXISTING]**: landed at the baseline.
- **[APPROVED]**: in an approved design (including this one's decision record, [§4](#decision-record)) but not landed.
- **[PROPOSED]**: new in this document; detail confirmed when its phase starts.

---

<a id="problem"></a>
## 1. Problem

The cloud control plane is one `goobers` daemon process on one ReadWriteOnce (RWO) volume [EXISTING]:
- It holds the run journals, `claims.json`, scheduler state and seven SQLite databases on that volume. Exclusion is a `flock` on `up.lock` (`cmd/goobers/up.go:676-695`).
- The reference preparer forces `replicas: 1`, `strategy: Recreate` and a 90s grace (`deploy/reference/authenticated/main.go:559-565`). There was no PodDisruptionBudget anywhere in `deploy/` at the baseline.
- Temporal runs one replica (`deploy/reference/temporal/values.yaml:30`) on a Burstable, non-HA Postgres (`infra/bicep/modules/postgres.bicep:18-19,49-51`).

What that costs operators today:
- **Every deploy is an outage** of about 2 minutes, growing with run volume (k8s-infra-shape §4). During it there are no admissions, claims, trigger/webhook/HITL ingestion, journal emits, credential issuance or SSE.
- **Restarts burn stage attempts.** Plane clients give up after 45s/8 tries (`internal/planehttp/retry.go:42-48`, about 9–18s of real backoff), and pod artifact records after 3s (`cmd/goobers/dispatchexec.go:1300-1301`).
- **Node loss means a disk detach/attach** plus the default 300s unreachable toleration: roughly 7–12 minutes (estimate; detach time UNVERIFIED).
- **Worker rollouts kill in-flight stages.** The drain is 30s + 5m, then recorded child pods are deleted (`internal/dispatcher/child_custody.go:36-50`). Side-effecting stages never retry after worker loss (`internal/engine/retry.go:286-288`), so the run fails.
- **A latent trap shipped.** The base `api-deployment.yaml` set no strategy and no grace (`deploy/reference/goobers-system/api-deployment.yaml:11-15`); an adopter at `replicas: 1` gets a RollingUpdate that deadlocks on RWO plus `up.lock`.

**User story (end state).** *As an instance operator, I upgrade Goobers, rotate keys, and let AKS patch nodes during business hours. Runs, HITL waits and portal users see no errors and no restarted stages. If a node or a zone dies, admissions resume within a minute without my intervention. Nothing acknowledged is lost or done twice. The same DSL 3.0 workflow runs locally and in cloud.*

**User story (first slice, P0–P3).** *A deploy pauses admission for under a minute and costs no stage attempts. Worker rollouts and node drains never fail a stage. Losing the control-plane node costs a couple of minutes of admission, not ten, and needs no disk detach.*

---

<a id="tldr"></a>
## 2. TL;DR

1. **Seven committed phases; `main` stays releasable after each.** P0 fix-now, P1 extraction and seams, P2 worker re-attach and rainbow rollouts, P3 state off the RWO disk with a single controller and cutover, P4 engine parity (alongside P2–P3), P5 controller HA and per-role namespaces, P6 removal of mode 3 from `goobers` plus certification. Each phase has entry criteria and an exit gate of named tests ([§17](#phases)).
2. **Cloud is a separate binary, `goobers-cloud`, on Postgres and object storage.** `goobers` keeps modes 1/2 on files. Local behaviour is unchanged, proven by the existing suites; 45 of the 92 work issues change code compiled into `goobers` and each is listed ([§9](#local-impact)). An import rule keeps pgx, azblob and cloud packages out of `goobers`.
3. **Cloud is engine-only from its first release (D-2).** The cloud control plane never executes stages; there is no runner fallback. `goobers-cloud` refuses any workflow that the engine can't run: DSL 2.0 documents, the 8 engine-refused features until their P4 parity item ships, and stages that would need self execution. `goobers validate` reports this (`RUNTIME001`), and a cutover-readiness check lists every blocker per instance ([§8](#cloud-dsl-surface)).
4. **All 8 refused features are ported to the engine in P4 (D-1)**, reversing decision 005 R9. The order follows shipped usage: parallels and selection first (the only refused features used by the reference and example workflows), then maps, child workflows and parents, human gates, remote limits, outbox and experiment ([§8.5](#engine-parity)).
5. **Consequence: P4 items gate specific instance cutovers.** An instance cuts over in P3 only when all its workflows are engine-eligible. On `main` today that requires migrating to DSL ≥3.0 first (16 of 17 reference workflows and all 23 config-example workflows are DSL 2.0), and for the self-hosting reference gaggle it also requires parity for parallels and selection.
6. **Mode 3 inside `goobers` is frozen at P3 exit, deprecated for one minor release and removed in P6 (D-3).** `__dispatch-exec` and the stage contract stay in `goobers`.
7. **Correctness lives in the database; leadership is an efficiency layer.** Run IDs come from durable acceptance rows (as they already do on queued paths). A start-owner memo makes a zombie's duplicate start "not mine". The lease and its heartbeat are separate rows, so fenced transactions never block renew. The journal uses a narrow per-run counter row and no NOTIFY inside emit transactions ([§11](#mechanisms)).
8. **Plane clients ride a restart or DB failover** with a time-based retry budget, `min(240s, remaining stage deadline − 15s)`, no try cap; a bare 502/503/504 is ambiguous; exhaustion is always `infra` ([§11.5](#client-retry)). This ships in P0.
9. **Redaction stays byte-identical to today.** P3 keeps the in-memory scrubber in one writer. P5 uses per-run writer affinity with an in-memory registry handoff on SIGTERM. No secret material, raw or fingerprinted, is stored in the database ([§11.4](#scrubber)).
10. **Targets.** After P3: deploy pause 30–60s, node loss ~1.5–2.5 min with no disk detach, 0 stage attempts burned by restarts, 0 stage restarts from worker rollouts. After P5: controller handover ≤5s, failover ≤60s while Temporal is reachable, zero-downtime api rollouts, RPO 0 for acknowledged writes. After P6: zone loss ≤3 min and a published 99.9% SLO.
11. **Honest size:** 92 work issues (plus an epic and 7 phase trackers), about 155–210 PRs, about 66–112k non-test lines and 105–200k test lines, including 9 CI-infrastructure issues. The required-PR wall clock stays unchanged ([§18](#ci-plan)).

---

<a id="summary"></a>
## 3. Summary for humans

### 3.1 Phases at a glance [APPROVED]

| Phase | Goal | Issues | Exit gate (abridged) | What users and operators get |
|---|---|---|---|---|
| **P0** Fix now | Stop restarts burning attempts; remove the rollout deadlock; cache the Temporal CLI in CI | 9 | planehttp budget/classification table; manifest and preparer tests; merge-lock, replay-lock and grant-key tests; bounded drain; restart suite green | Restarts cost **0 infra attempts**; the shipped base no longer deadlocks; worker grace covers drain + settlement |
| **P1** Extraction + seams | No behaviour change. Make cloud buildable from importable packages; add the gates | 14 | `make ci` with no golden changes; backend conformance (file); closure rule; growth gate covers `cmd/goobers-cloud`; equivalence baseline | None (enabler). `cmd/goobers` shrinks by about 10–15k lines |
| **P2** Re-attach + rainbow | Worker rollouts and drains never kill or fail a stage | 8 | CHAOS-13, UPGRADE-11 (nightly kind); re-attach unit and replay tests; worker readiness | **0 stage restarts** from worker rollouts; **0 failed side-effecting stages** from drains |
| **P3** State off RWO, single controller, cutover | Postgres + object storage; one `control` pod with fast restart; engine-only admission; HA Temporal; cutover | 27 | cloud-pg store suites, fence and start-fence tests, N-1/N migration, local↔cloud equivalence; CHAOS-4/6/9/10; RESTART-RTO; one instance cut over and stable 14 days | Deploy pause **~30–60s**; node loss **~1.5–2.5 min**, no disk detach; DB failover ridden out |
| **P4** Engine parity (∥ P2–P3) | Engine runs the whole GA DSL; cutover readiness per instance | 16 | per-feature parity, replay and recovery tests; empty engine-unsupported set; every reference workflow green on the engine | Each item unblocks the cloud cutover of instances that use that feature |
| **P5** Controller HA + N api + namespaces | Hot standby, epoch fence everywhere, N api replicas, per-role namespaces | 10 | CHAOS-1/2/3/7/8/10; netpol matrix on an enforcing CNI; run-owner affinity and registry-handoff tests | Handover **≤5s**, failover **≤60s** (Temporal reachable); zero-downtime api, webhook, HITL and SSE ingestion |
| **P6** Mode-3 removal + certification | Remove mode 3 from `goobers`; GC, SLOs, soak; outbox only if measured need | 8 | UPGRADE-12, CHAOS-5/14/15, `omes` sizing, 24h soak, PITR drill, GC property test | Zone loss **≤3 min**; published **99.9%** SLO; one mode-3 implementation |

Ordering:

```
P0 ──► P1 ──────────────► P3 ──► P5 ──► P6
  └──► P2 (P2-05 before P3) ┘      ▲      ▲
P4 runs alongside P2–P3; each item unblocks specific instance cutovers; P4 exit gates P6
```

### 3.2 What changes for whom

- **Local users (modes 1/2).** Behaviour is unchanged. `goobers validate` gains one informational finding line for lanes that would not run in cloud. Mode 2 (single-pod cloud `goobers`) is frozen and gets only the P0 fixes.
- **Operators running mode 3 inside `goobers` today.** It keeps working, including today's runner fallback, until P3 exit; then it is frozen (fixes only) with a deprecation warning pointing at `goobers-cloud`, and it is removed in P6 after one released minor.
- **Cloud operators.** A one-time cutover to a fresh `goobers-cloud` install, with no history import (the old instance stays read-only for its retention window). The cutover is allowed only when `goobers-cloud cutover check` (or `goobers validate --target cloud` on the old instance) reports no blockers.
- **Workflow authors targeting cloud.** Cloud runs DSL ≥3.0 workflows on the Temporal engine only. Migrate with `goobers fix --to 3.0`, declare `runsOn` for agentic gates, and check with `goobers validate --target cloud`. Features still being ported are refused with the name of the parity item that unblocks them.

### 3.3 What does not change

- The journal stays product-owned and decoupled from Temporal history (Goobernetes D0, DS4). Every emit carries an idempotency key.
- One scheduler, no Temporal Schedules (Goobernetes D5, #6447).
- DS6 renew-before-reap ordering and DS8 reachability GC.
- The pod-facing plane contracts (`internal/apicontract`, additive changes only).
- The stage contract binary is `goobers` (dsl-3.0 D6, "base runner contract").
- DSL 2.0 stays the frozen, local-runner-only backward-compatibility contract (dsl-3.0 D14).

### 3.4 Is it worth it?

The control plane is not the main SLO limiter: GitHub, LLM providers and Temporal bound availability more. At ten deploys a month, today's 2-minute pause costs about 0.05% of a month. The PO has committed to the end state, so the justification is per phase:
- **P0–P3 capture most user-visible value at about a third of the cost:** no burned attempts, no killed stages, no disk detach.
- **P5** buys ~30–90s per deploy or node loss, zero-downtime webhook/HITL/SSE ingestion, and the precondition for N-way api scaling.
- **P4 + P6** give one DSL that runs everywhere and one mode-3 implementation instead of two drifting ones.

Entry criteria ([§17](#phases)) stop a later phase from starting before the earlier one is proven in production.

---

<a id="decision-record"></a>
## 4. Decision record

D-1…D-10 are this document's decisions (not the Goobernetes D0–D12 or dsl-3.0 D1–D18). D-1…D-3 were decided by the PO on 2026-10-21. D-4…D-10 take the recommended default; each is **"default adopted, confirm at phase start"**, and a change at confirmation re-plans only the affected issues.

| # | Decision | Outcome | Phase | Consequences in this document |
|---|---|---|---|---|
| **D-1** | Reverse decision 005 R9 ("not being ported") for the engine-refused features | **APPROVED (PO 2026-10-21).** All 8 are ported in P4: human gates, parallels, maps, selection, experiment, `task.outbox`, remote `task.limits`, child-workflow parents (`task.childWorkflows`) | P4 | §8.5 parity track; HA-P4-03 records the decision; the R9 comment in `internal/engineadmission/refusal.go:15-20` is removed by HA-P4-16 |
| **D-2** | Runner fallback in cloud | **DECIDED (PO 2026-10-21): no runner fallback in cloud.** Cloud refuses any workflow that uses an engine-unsupported feature until its P4 item ships; `goobers validate` flags it (`RUNTIME001`). The control plane never executes stages. `selfExecution: allow` is not needed in cloud; `deny` is enforced. The in-controller fallback and run-host pods are dropped. A P3 cutover is possible only for instances whose workflows are all engine-eligible, so P4 items gate specific instance cutovers; a cutover-readiness check lists the blockers | P3, P4 | §8 (engine-only admission, readiness check), §8.5 ordering by usage, §11.4 (P3 single writer), §14 (no stage execution in `control`), §20 #6/#7 |
| **D-3** | Mode 3 inside `goobers` | **APPROVED (PO 2026-10-21).** Frozen at P3 exit, deprecated for one released minor, removed in P6. `__dispatch-exec` and the stage contract stay in `goobers` | P3 exit, P6 | §9.2; HA-P3-26, HA-P6-01 |
| **D-4** | Departure from Goobernetes D1 "one binary": two binaries (`goobers`, `goobers-cloud`), one DSL, one repo | **Default adopted, confirm at phase start** | P1 | §20 #6. The DSL promise is kept by P4, the runtime-support column and the equivalence test |
| **D-5** | Redaction: per-run writer affinity in P5, no shared fingerprint registry | **Default adopted, confirm at phase start.** Residual: a crashed owner's in-flight credentials are unknown to its successor, which is today's daemon-restart gap, not a new one | P5 | §11.4; HA-P5-03, HA-P6-03 |
| **D-6** | Database: Azure PostgreSQL Flexible Server, General Purpose, zone-redundant (Azure reference) + CloudNativePG (cloud-agnostic reference). Not Azure SQL | **Default adopted, confirm at phase start.** Cost on the order of a few hundred USD/month per instance before storage (UNVERIFIED) | P3 | §12, §21; HA-P3-21 |
| **D-7** | Object storage: azblob only now; an S3-compatible store only if a non-Azure target is committed | **Default adopted, confirm at phase start** | P1 record, P3 | HA-P1-01, HA-P3-13 |
| **D-8** | Telemetry in cloud: port only `first_success_milestones` and the 4 defect-nomination aggregates; the rest goes to OTLP to a user-configured collector | **Default adopted, confirm at phase start** | P3 | HA-P3-15 |
| **D-9** | HITL hold maximum in cloud: cap `engine.hitl.window` at 7d, and test AUTO_UPGRADE at hold resume | **Default adopted, confirm at phase start** | P5 (cap usable from P3) | §10.5; HA-P5-09 |
| **D-10** | Cutover without history import; the old instance stays read-only for its retention window | **Default adopted, confirm at phase start** | P3 | §12.5; HA-P3-25 |

---

## 5. Goals, non-goals, targets

**Goals** (all committed):
- G1: HA and rolling updates for the cloud control plane (api, controller, workers), Temporal and the database.
- G2: All authoritative state off node-local disk, in Postgres and object storage. Node-local disk holds caches only.
- G3: Exactly-once invariants (no double-fired trigger, no double claim, no journal seq gap or duplicate) enforced **by the database**, with leadership as an efficiency layer.
- G4: Mature k8s posture: PDBs, spread, priority classes, probes, bounded drains, per-role namespaces and identities.
- G5: **One DSL.** The same GA DSL 3.0 workflow runs locally and in cloud (P4).
- G6: Chaos and upgrade suites that gate each phase.

**Non-goals:**
- HA for modes 1/2. Mode 2 is frozen (P0 fixes only).
- Executing stages, or running the local runner, in the cloud control plane (D-2).
- Multi-region active/active. Region DR stays backup-based.
- Horizontal admission scaling. One active controller is enough; the bucket-shard path is documented in [§21](#alternatives).
- Importing file-backed history (D-10).
- Temporal multi-cluster replication.

**Targets by phase [APPROVED]:**

| Target | Today | After P0 | After P3 | After P5/P6 | Gating test |
|---|---|---|---|---|---|
| Admission pause per deploy | ~2 min | ~2 min | **30–60s** | **≤5s** (handover) | RESTART-RTO, CHAOS-2 |
| Stage attempts burned by a control-plane restart | yes (45s/3s windows) | **0** | 0 | 0 | CHAOS-6 |
| Control-plane node loss | ~7–12 min (est.) | same | **~1.5–2.5 min** (k8s detection ~40–50s + toleration 30s + start + rebuild) | **≤60s** given Temporal reachable | CHAOS-1 |
| Zone loss | outage | outage | ~2.5–4 min (single replica rescheduled; DB zone-redundant failover) | **≤3 min** | CHAOS-15 |
| Stage restarts from worker rollout | every interrupted stage | stages longer than the settlement window | **0** (P2) | 0 | CHAOS-13 |
| RPO, acknowledged writes | 0 on disk; minutes on node loss if the disk is lost | same | **0** (sync zone-redundant / CNPG `dataDurability: required`) | 0 | CHAOS-4 |
| RPO, operator PITR restore | n/a | n/a | ≤5 min (WAL backup), documented | same | restore drill |
| DB failover window ridden out | n/a | n/a | 60–120s absorbed by the client budget | same | CHAOS-4 |
| API plane availability (monthly, 1-min buckets) | unmeasured | — | — | **99.9%** published after the soak | SLO dashboard |

RTO statements are qualified: controller failover ≤60s holds *given Temporal is reachable*. During a Temporal outage admission is paused by design, and renewal treats "unknown" as live (CHAOS-5).

---

<a id="current-state"></a>
## 6. Current state [EXISTING]

### 6.1 Mechanisms that assume one daemon

| Class | Members | Why it breaks under N replicas or rolling overlap | Target |
|---|---|---|---|
| **C1 Host exclusion** | `up.lock` flock singleton, lock-file mtime heartbeat, M5 owner/generation tripwire, file `merge.lock` | flock is per kernel and unreliable on RWX (#2854). No epoch or fence. M5 is read-then-write and only *detects* (`stateguard.go:92-123`) | DB lease row + epoch. M5 retired in cloud |
| **C2 Correctness state in process memory** | admission slots and budgets, MaxOpenPRs, provider quota, pacing maps, terminal-report `sync.Map` (`internal/terminalreport/flight.go:18-25`), pod-token Registry, credential grant key (`cmd/goobers/credentialrefresh.go:80-85`), feed waiters, intervention `active` map | Each replica would enforce caps independently, or a token minted on A fails on B | Leader-only memory with persisted observations, or a shared row |
| **C3 Single-writer files on RWO** | `claims.json`, scheduler-state keys, run journal flock + seq, instance-log tail-read seq, config dir swap, recovery records, `pending-*` drops | Whole-file rewrite under flock; seq allocated in memory | `control` / `journal` tables with row CAS and counter rows |
| **C4 Already CAS-shaped SQLite** | trigger queue, webhook receipts, schedule cursor, cancel receipts | Correct shape, wrong medium | Port the tables; add `owner_epoch` to `dispatching` |
| **C5 "Not mine ⇒ dead" boot logic** | crash-resume of non-terminal runs (`up.go:1665-1700`), scratch reap (`up.go:1370-1385`), tracked-probe "authoritative not-live" (`renewal.go:41-45`), trigger `UncertainIDs` boot snapshot (`triggerqueue/store.go:234-259`) | Correct only for a singleton. A booting replica would declare a live peer's work orphaned and double-dispatch; porting the store alone turns these into live bugs | Runs only on lease acquisition, keyed by owner epoch |
| **C6 In-process run ownership** | `self` lanes, runner-path HITL/cancel (`writeplanes.go:268-290`), the `awaitInto` goroutine per engine run + 7 terminal hooks (`cmd/goobers/enginestarter.go:240-263`, `engineterminalhooks.go:21-172`) | A run is owned by whichever process re-attached it, so hooks fire there or twice | Engine-only cloud (D-2); hooks fenced (P5) |
| **C7 Deploy shape** | `replicas: 1`, Recreate, no PDB, no spread or priority | Outage per deploy or drain | [§14](#k8s-shape) |
| **C8 Host-local discovery** | API address file, self-update stop file | Meaningless across pods | Service DNS; not in cloud |

**Already-safe templates:** Temporal run start with `WorkflowID == RunID` + `REJECT_DUPLICATE` (`internal/engine/starter.go:126-131`); per-attempt `DispatchOne` ID (`internal/engine/dispatchone.go:32-93`); HITL Update deduplicated by `UpdateID` (`internal/engine/hitlclient.go:130-136`); durable trigger and webhook receipts (`internal/triggerqueue/source_start.go:78-126`); the `sharedclaim` owner token (`internal/sharedclaim/lease.go`); stateless HMAC pod tokens (`internal/podauth/signed.go`).

### 6.2 Store inventory

| Kind | Stores | Size/volume (self-hosting instance, 2026-07-29) | Cloud target |
|---|---|---|---|
| Authoritative append logs | run journals (`events.jsonl` + derived `state.json`); instance log `scheduler/events.jsonl` (flock + tail-scan seq); gaggle-health log | journal 191 MB / 29.8k runs; instance log 324 MB / 157k records | `journal.*` tables |
| Authoritative mutable control | `claims.json`, scheduler-state keys, M5 files, `accepted-triggers.db`, `cancellation-receipts.db`, terminal-report intents, surrender docs, recovery metadata, config generations, workspace custody | small | `control.*` tables |
| Content-addressed bytes | blobstore (artifacts, spans 2.26 GB / 70k files, inputs, kits, config archives), recovery bundles | ≥100 Gi PVC in cloud | object storage |
| Derived | `read.db`, `intake.db`, `telemetry.db` (547 MB, ~40 tables), export spools | — | `readmodel` schema; telemetry per D-8 |
| Caches | git mirrors and worktrees, API read cache, merge-policy cache, extracted config trees, harness home | — | node-local emptyDir, never the only copy of a fact |
| Temporal history | external PG `temporal` / `temporal_visibility` | modest | unchanged contract; HA ([§13](#temporal)) |

The seven SQLite DBs use `SetMaxOpenConns(1)` + `_txlock=immediate` + flock, so the PVC must be RWO; RWX is a false green (#2854). **Moving state off the PVC is the only path to more than one replica.**

### 6.3 Four facts that shape the plan

1. **At the plan baseline, the engine refused 8 DSL features.** `internal/engineadmission/refusal.go` covered parallels, selection, maps, child workflows, experiment, outbox, remote limits and human gates. D-1 reverses that baseline incrementally; remote token/cost limits now use trusted supervisor usage.
2. **The runner cannot place stages in pods.** `DispatchOne` is the seam by which a non-engine driver would place one stage attempt in a pod, but "nothing constructs a client for it yet" (`internal/engine/dispatchone.go:14-30`). dsl-3.0 says `internal/runner` "has no gate dispatch arm and will not get one" (`docs/design/dsl-3.0.md:205-215`). A runner-driven lane executes its stages in-process on the host running the runner. That is why D-2 leaves cloud with no runner at all rather than a remote-dispatching one.
3. **Every DSL 2.0 lane self-pins.** Pre-3.0 placement emits task-only rows and no gate rows: "a 2.0 gate cannot carry runsOn, so every 2.0 gate stays in the control plane" (`internal/workflow/placement.go:46-55`). The daemon keeps such lanes on the runner (`cmd/goobers/engineselection.go:60-130`), and child-workflow parents go through `containedParentSelection` (`:240-246`). In an engine-only cloud, **a 2.0 workflow cannot run at all**, regardless of the 8 features. This matches dsl-3.0 D14 ("2.0 is local-runner-only").
4. **Run IDs already come from durable acceptance rows on every queued path.** Triggers (`internal/triggerqueue/store.go:131-197`: "Assigned run IDs are the random suffix of the durable acceptance ID"), source batches (schedules, webhooks, item/backlog workers, signals; `source_start.go:103-111`) and child starts (`children.go:218`). Random minting remains only on direct paths: `dispatchRunID("")` (`localscheduler/dispatchidentity.go:10-13`, via `scheduler.go:2701`), `childadmission.go:126`, `cmd/goobers/run_continue.go:168` and `runner/child_quiescence.go:32`.

---

<a id="architecture"></a>
## 7. Target architecture

### 7.1 Shapes by phase [APPROVED]

| | Today (mode 3 in `goobers`) | After P3 (first cloud shape) | End state (P5/P6) |
|---|---|---|---|
| Control-plane process | `goobers up` daemon, 1 pod, RWO, Recreate; runs runner lanes in-process | `goobers-cloud control`: api + controller roles **in one process**, 1 replica, Recreate, DB lease + epoch fence, no PVC, **never executes stages** | `goobers-cloud api` ×N (HPA 3..8) + `goobers-cloud controller` ×2 (1 active, hot standby) |
| State | files + 7 SQLite DBs on RWO; blobs on RWX | Postgres (`journal`, `control`, `readmodel`) + object storage; node-local caches only | same |
| Lanes the engine refuses | run on the runner in the daemon | **refused** at config apply and validate; unblocked per feature by P4 | none (P4 complete) |
| Engine runs | daemon `awaitInto` goroutine + 7 terminal hooks | same code in the `control` pod; re-attached after restart | leader-owned `awaitInto` with **fenced hooks** (P5); optional terminal intent + outbox (P6) |
| Workers | 1 pod, Recreate, RWO mount | rainbow Worker Deployments, DB-less, re-attach (from P2) | + KEDA / external-metric scaling |
| Temporal | 1 replica, Burstable PG | **HA fresh install** (3/3/3/2, PDBs, 512 shards) on the same HA Postgres server, separate DBs | same |
| Namespaces | `goobers-system` | `goobers-cloud` (control, workers, migrate) + `goobers-temporal` + stage namespaces | 9 per-role namespaces ([§14.2](#namespaces)) |
| Scrubber | in-process registry | in-process registry in the single writer (byte-identical) | per-run writer affinity ([§11.4](#scrubber)) |

### 7.2 End-state topology [APPROVED]

```
                        Internet / corp network
                                  │  HTTPS (portal, API, GitHub webhooks)
                         ┌────────▼─────────┐
                         │ Ingress / Gateway│  (SSE: buffering off, read-timeout ≥ 1h)
                         └────────┬─────────┘
 ns goobers-api                   │ Service goobers-api (no affinity; run-owner proxy inside)
   ┌──────────────┬───────────────┼──────────────┐
   │ api-0        │ api-1         │ api-2        │  stateless; HPA 3..8; PDB maxUnavailable 1
   │ planes, portal, SSE, webhooks, credential mint; run-scoped planes served by the run's owner
   └──────┬───────┴───────┬───────┴──────┬───────┘
          │ SQL (PgBouncer 6432; notifier on 5432)     ▲ plane calls (run-scoped bearer)
 ns goobers-controller    │                            │
   ┌──────────────────┐   │                ns goobers-stages-<gaggle>
   │ controller-a  ★  │   │                  ┌──────────────────────────┐
   │ L1 tick/admission│   │                  │ stage pod (one/attempt)  │──► egress (allowlist)
   │ L2 renew/reap/   │   │                  └──────────▲───────────────┘
   │    stall/reattach│   │                             │ create/watch/adopt
   │ L3 config apply  │   │                ns goobers-workers
   │ projector, GC,   │   │                  ┌──────────────────────────┐
   │ awaitInto + hooks│   │                  │ worker vN  (rainbow)     │  no DB credentials
   ├──────────────────┤   │                  │ worker vN+1              │
   │ controller-b (sb)│   │                  └──────────┬───────────────┘
   └───────┬──────────┘   │                             │ gRPC
           │ lease + heartbeat rows (epoch, DB clock)   │
 ns goobers-data / managed PG                ns goobers-temporal
   ┌───────▼──────────────────────────┐      ┌──────────────────────────┐
   │ Postgres (ZR HA / CNPG ×3 quorum)│◄─────│ frontend×3 history×3     │
   │  db goobers_<inst>: journal|     │      │ matching×3 worker×2      │
   │            control|readmodel     │      └──────────────────────────┘
   │  db temporal, temporal_visibility│
   └──────────────────────────────────┘      Object storage (Blob): blobs/, recovery/
 ns goobers-ops: migrate Job (PreSync)       ns goobers-observability: OTel collector, metrics
 ★ = lease holder
```

| Role | Replicas | Runs | Lease | DB identity | k8s token |
|---|---|---|---|---|---|
| `api` | ≥3 (P5) | request planes, portal/read, SSE, webhooks, credential mint, run-owner planes | `run_owner:<run>` rows | `goobers_api` | none |
| `controller` | 2 (P5) | L1 tick/admission/cron, L2 renew/reap/stall/re-attach, L3 config apply, projector, GC, `awaitInto` + terminal hooks for engine runs. **Never executes stages** | `control`, `projector`, `gc` | `goobers_controller` | none |
| `control` (P3 only) | 1 | api + controller in one process. **Never executes stages** | `control` | both grants | none |
| `worker` | N per version | Temporal workflows/activities, pod dispatch, re-attach | none | **none** | pod create/watch/delete in stage namespaces |
| `migrate` | Job | DDL, backup/PITR mark | `migrate` + advisory lock | `goobers_owner` | none |

### 7.3 Leader set [APPROVED]

One `control` lease covers:
- **L1:** scheduler tick, cron/demand polls, pacing, admission slots, MaxOpenPRs, quota budgeting, acceptance drain.
- **L2:** renew, `RecoverExpired`, stalled sweep, engine re-attach, uncertain-acceptance reconcile, instance-wide retention.
- **L3:** config apply, `applied_generation`, generation pruning.

One lease rather than three, because L1 and L2 interact (slot release follows terminalization; reap must follow renew). The projector and blob GC take separate leases (`projector`, `gc`) so they can move later; by default the active controller holds all of them.

### 7.4 Request flow (end state)

- **Journal emit:** stage pod → any api replica → proxied one hop to run R's owner (P5) → scrub in memory → fenced emit transaction ([§11.3](#journal-write-path)) → 2xx after COMMIT. A lost response is retried with the same key and returns the original seq.
- **Trigger / webhook:** client or GitHub → any api replica → `INSERT control.acceptance` (keyed by `Idempotency-Key`, or a source receipt derived from `X-GitHub-Delivery`) → 2xx. The controller drains with `FOR UPDATE SKIP LOCKED`.
- **Admission and start** (controller, epoch E): L1 picks accepted rows → fenced `accepted → dispatching` stamping `owner_epoch = E` → reserve the claim row under the fence → `StartWorkflow(ID = run_id, REJECT_DUPLICATE, memo = start-owner)` → on `AlreadyStarted`, the start-owner check ([§11.1](#start-fence)).
- **HITL:** human → any api replica → Temporal Update with `UpdateID = RequestID` [EXISTING path]; the journal marker goes to the DB.
- **Terminal:** the controller's `awaitInto` observes the close → fenced effect intents (P5) → claim release, parking, branch prep, notifications, terminal reports.

---

<a id="cloud-dsl-surface"></a>
## 8. Cloud DSL surface: engine-only admission and engine parity

### 8.1 The problem [EXISTING]

A workflow that runs locally can be refused by the engine, for two independent reasons: it is a DSL 2.0 document (every 2.0 lane self-pins, [§6.3](#current-state) fact 3), or it declares one of the 8 refused features (fact 1). In mode 3 inside `goobers` that is invisible, because the lane silently stays on the runner. In cloud there is no runner (D-2).

Shipped usage on `main` at the baseline:
- The reference gaggle has 17 workflows. 16 are DSL 2.0; `quality-spot-check` is DSL 3.1 (preview).
- `config-examples/` has 23 workflows across its gaggles, all DSL 2.0.
- At the baseline, refused features declared by those 40 workflows were `spec.parallels` in 4 (`quality-sprint`, `quality-spot-check`, and `work-nomination` in both example gaggles) and `spec.parallels.selection` in 1 (`quality-spot-check`). None declares `spec.maps`, `task.childWorkflows`, `task.experiment`, `task.outbox`, remote `task.limits.maxTokens/maxCostUSD`, or a human gate.

<a id="engine-only-admission"></a>
### 8.2 Rule: cloud is engine-only from its first release [APPROVED, D-2]

- **The cloud control plane never executes stages.** `goobers-cloud` enforces `placement.selfExecution: deny` at boot; its runner inventory has no self runner; `selfExecution: allow` is not a valid cloud setting. `internal/runner` is not in `goobers-cloud`'s dependency closure (asserted by `TestGoobersCloudClosureExcludesRunner`).
- **A workflow is cloud-eligible only when all of these hold:**
  1. `dslVersion` ≥ 3.0 (2.0 is local-runner-only, dsl-3.0 D14);
  2. `engineadmission.RefusePlacedDefinition` returns nil against the cloud runner inventory: no engine-unsupported feature or human gate;
  3. no stage pins to self, and every agentic gate has a placement row (`runsOn`).
- **Where it is enforced (defence in depth):**
  1. **Validate time:** findings `RUNTIME001`–`RUNTIME003` ([§8.3](#runtime-support-column)).
  2. **Config apply:** `goobers-cloud` refuses a config generation that contains an ineligible workflow. The running generation stays applied, and `goobers-cloud config status` names every blocker. A config cloud can't fully run is never half-applied.
  3. **Admission:** the scheduler never admits an ineligible workflow (guard for configs applied before a binary downgrade).
  4. **Run start:** the engine's existing start-time refusal (`RefuseFeatures`, `RefuseHumanGate`) stays as the last layer.
- **Lifting a refusal.** Each P4 item removes its feature's refusal and flips its runtime-support row in the same PR. The next config apply accepts workflows that use it, with no cloud flag.
- **The lock.** Cloud accepts DSL 3.0 and 3.1 documents as the shipped interpreters validate them; the DSL 3.0 stable lock (#7590) is not a precondition for P3. If the lock changes a semantic, affected workflows are re-migrated with `goobers fix` and the affected P4 item is re-planned.

<a id="runtime-support-column"></a>
### 8.3 Runtime-support column and validate findings [APPROVED] (HA-P4-01)

- **Registry.** `internal/workflow/v_3_0/features.go` `Feature` gains `Runtimes []RuntimeSupport{Runtime: "runner"|"engine", Level SupportLevel, Since string}`. The same column goes into v_3_1 and the supportmatrix export (`internal/supportmatrix/supportmatrix.go`), so `goobers dsl support` and the agent-toolkit manifest show it.
- **One source of truth.** `TestRuntimeSupportColumnMatchesEngineRefusals` asserts that the features marked `engine: unsupported` equal `engineadmission.UnsupportedFeatures` ∪ {human gate}. Neither can drift.
- **Findings:**

| Code | Meaning | Local instance | Instance with `engine:` (mode 3 in `goobers`) | Cloud target (`--target cloud`, `goobers-cloud`) |
|---|---|---|---|---|
| `RUNTIME001` | lane declares an engine-unsupported feature; names the feature and the P4 issue that unblocks it | info | warning (lane stays on the runner, as today) | **error** |
| `RUNTIME002` | lane would need self execution: a self-pinned stage, or an agentic gate without `runsOn` | — | warning | **error** |
| `RUNTIME003` | lane is DSL < 3.0 (local-runner-only); suggests `goobers fix --to 3.0` | — | — | **error** |

Selection-time reasons in `engineselection.go` keep their operator-facing text; the findings move the same facts to validate time. Local validate goldens change deliberately for the new info line.

<a id="cutover-readiness"></a>
### 8.4 Cutover-readiness check [APPROVED] (HA-P4-02, HA-P3-25)

Because a P3 cutover needs every workflow to be cloud-eligible, operators need the full list of blockers before they plan one.
- **`goobers validate --target cloud`** runs against an existing instance root (local or mode 3) and reports every `RUNTIME001`–`RUNTIME003` error for every workflow in every gaggle, grouped by blocker:
  - per workflow: each blocking feature or placement fact, with the stage and DSL path;
  - per blocker: the unblocking action. A config fix (`goobers fix --to 3.0`, add `runsOn`) or the P4 issue that ports the feature;
  - a summary line: `cloud-ready` or `blocked: N workflows, M blockers (K config fixes, J waiting on P4 items: …)`;
  - exit status non-zero while blocked; `--format json` for automation.
- **`goobers-cloud cutover check`** runs the same library against the target install's config and runner inventory, plus install-side checks (schema compatible, lease row present, admissions paused, bucket reachable). HA-P3-25's runbook makes a clean result step 0 of the cutover.
- **It drives P4 priority.** At P4 start, and at each P4 item's start, run the check on every instance planned for cutover and confirm the order in [§8.5](#engine-parity).

Expected result for the reference gaggle at the baseline: `RUNTIME003` for the 16 DSL 2.0 workflows (#7616 migrates them; they also report `RUNTIME002`, because every 2.0 lane self-pins), and `RUNTIME001` for `quality-sprint` (parallels) and `quality-spot-check` (parallels, selection).

<a id="engine-parity"></a>
### 8.5 Engine-parity track (P4): order and approach [APPROVED, D-1]

**Order.** By shipped usage on `main` (§8.1), then by how widely a refusal bites in cloud, then by reuse of machinery already built:

| Order | Feature | Shipped usage | Why here | Issues |
|---|---|---|---|---|
| 1 | `spec.parallels` (static fan-out/fan-in, `@join`, branch-qualified inputs) | **4 workflows** (`quality-sprint`, `quality-spot-check`, `work-nomination` ×2 examples) | Most used; blocks the self-hosting gaggle and both example gaggles | HA-P4-04, HA-P4-05 |
| 2 | `spec.parallels.selection` | **1 workflow** (`quality-spot-check`) | Second most used; small once parallels exist | HA-P4-06 |
| 3 | `spec.maps` (data-driven fan-out) | 0 | Reuses the parallels branch machinery, so it is cheapest next | HA-P4-07 |
| 4 | `task.childWorkflows` and child-workflow **parents** | 0 (children are agent-generated at runtime) | A detailed draft design exists; parents are the remaining `containedParentSelection` fallback | HA-P4-08, HA-P4-09, HA-P4-10 |
| 5 | Human gates (`evaluator: human`) | 0 | GA HITL primitive likely in adopter workflows; needs a new Update protocol | HA-P4-11, HA-P4-12 |
| 6 | Remote `task.limits.maxTokens/maxCostUSD` | 0 | **Landed:** trusted supervisor usage now enforces token and cost caps for remote stages | HA-P4-13 |
| 7 | `task.outbox` (workspace file export, #1552) | 0 | Contained change to the surrender protocol | HA-P4-14 |
| 8 | `task.experiment` (bandit arm assignment) | 0 (the `implementation-pre-review-experiment` lane is an A/B lane by name but declares no `task.experiment`) | Least used; needs a new plane route and store seam | HA-P4-15 |
| — | Qualification sweep against the locked DSL 3.0 surface | — | Waits for #7590/#7616 | HA-P4-16 |

The self-hosting instance's own configuration lives outside this repository, so this ranking uses the in-repo reference gaggle and config examples. The readiness check ([§8.4](#cutover-readiness)) re-ranks against real instances at each item's start.

**Common rules for every item:**
- the refusal is removed only in the item's last PR, together with the support-column flip;
- a `GetVersion` gate with a captured pre-change history replay test;
- a runner↔engine journal parity row (`parity_row_*_test.go`) and equivalence-corpus entries;
- recovery tests for worker loss mid-feature;
- `internal/engine` is changed by one open PR at a time.

**Approach per feature:**

| Feature | Approach | Prerequisites |
|---|---|---|
| Parallels | Branches run as concurrent coroutines inside the Run workflow (`workflow.Go`), each walking its branch sub-graph and dispatching stage activities as today; the join is a deterministic barrier; the journal numbers events `(run, branch, seq)` (the `branch` column already exists). Branch-qualified inputs and `@join` match `internal/runner/parallel_run.go`. **No dependency on child workflows** | HA-P4-03 record |
| Selection | Evaluated as a deterministic step before fan-out; the selection result is frozen in history; unselected branches are journaled as the runner does | parallels |
| Maps | Roster frozen at map start as a blob digest in history (the fan-out contract's source-freezing), one branch per item, bounded concurrency, the join and integrity checks (`workflow_integrity`) | parallels |
| Child workflows + parents | Per the engine-child-workflows draft, phase 1: frozen proposal bytes under `SourceDigest`, committed child origin carried from `emitPending` into dispatch (no projection ordinal), Temporal child workflows with one active path; parents on the engine, removing `containedParentSelection` | HA-P4-03 record |
| Human gates | New Update `goobers.gate-decision.v1`: `UpdateID = RequestID`; the validator binds (gate name, gate occurrence ordinal, terminal generation); the hold is a workflow timer bounded by the HITL window; `gate.pending` / `gate.decided` journaled via the emit plane; `approve` / `override` / `deny` route to the Update for engine runs. This is the "occurrence-bound Temporal signal" the refusal text asks for (`refusal.go:243`) | gate-occurrence identity in the journal; HA-P4-03 record |
| Remote limits | The trusted supervisor in the stage pod (`__dispatch-exec`, a Goobers binary, not workflow-authored) reads the harness adapter's usage, reports it in an additive surrender field, and enforces the in-stage cap by terminating the harness. The walk charges cumulative usage under the existing `cumulative-agentic-usage` gate. **Trust model:** usage is "supervisor-measured", the same class as the local runner's measurement; HA-P4-03 records the ruling | surrender field (contract version bump, N/N-1 tolerant) |
| Outbox | `__dispatch-exec` collects declared outbox paths after the stage, uploads them as content-addressed blobs, and lists them in an additive `outbox` surrender manifest; the walk records `artifact.recorded` (kind `outbox`) at the runner's paths | additive surrender field |
| Experiment | Arm assignment in a local activity whose result is recorded in history (replay-stable); observations through a new closed-parameter plane route to a bandit store seam (file store locally, a PG table in cloud). Workers keep no DB credentials | plane route; bandit store seam |

**Amendment to the engine-child-workflows draft.** That draft sequences child workflows (its phase 1) before parallels, selection and maps (its phase 2). This plan reverses the order by usage and builds parallels without child workflows. The draft's composition of fan-out inside generated children stays in its phase 2. HA-P4-03 records the reorder.

**Before and after the lock.** All P4 implementation proceeds before the DSL 3.0 lock. HA-P4-16 (qualification) waits for it, because the lock may still change these features' semantics; any such change re-plans the affected item. HA-P4-01 and HA-P4-02 have no lock dependency. dsl-3.0's "the runner has no gate dispatch arm and will not get one" is **not** reversed.

### 8.6 Alternatives for the parity gap (rejected by D-2)

| Option | Verdict |
|---|---|
| Runner fallback in-process in the P3 `control` pod (`selfExecution: allow`, today's mode-3 behaviour) | **Rejected.** The control plane would hold harnesses, workspaces, credentials and stage CPU (the k8s-infra-shape §7.1 cgroup hazard), and every later phase would have to carry it |
| Run-host pod per run, leased by a Temporal workflow, running the local runner with in-process stages | **Rejected.** A second execution path to build, secure, fence and then delete. Nothing else needs it, so it is not built |
| Wire `DispatchOne` so the runner walk stays in the controller and stages go to pods | **Rejected.** The runner has no gate dispatch arm by design; lanes with agentic gates would still be refused, and failover would move running walks |
| Engine-only from P3, refusing ineligible workflows until parity | **Chosen (D-2).** One execution path in cloud from day one; P4 items gate specific cutovers |

---

<a id="local-impact"></a>
## 9. Local (modes 1/2): what changes in `goobers`

**Claim:** *local behaviour is unchanged, proven by the existing suites* (`make ci`, journal goldens and digests, `conformance_test.go` and `parity_row_*_test.go`, `help.golden`, man pages, `deploy_reference_*_test.go`, replay tests). Shared code is refactored under local, so the binary's contents change; behaviour does not, except where a row below says so.

### 9.1 Every issue that touches code compiled into `goobers`

| Issue | What changes in `goobers` | Behaviour change? | Proof |
|---|---|---|---|
| HA-P0-02/03 *(fixtures only)* | reference manifests + preparer, tested by `cmd/goobers/deploy_reference_*_test.go` | deploy shape only | manifest and preparer tests |
| HA-P0-04 | merge-lock domain (`claimsclient`, claim ledger) | yes (bug fix: file and plane domains exclude) | `TestClaimLedgerFileAndPlaneMergeLocksShareExclusion` + claims suites |
| HA-P0-05 | terminal-report replay owner (`internal/terminalreport`) | yes (bug fix) | `TestReplayLockExcludesProcessesAndReleasesAfterCrash` + #7671 suites |
| HA-P0-06 | credential grant key derivation (`cmd/goobers/credentialrefresh.go`) | yes (grants survive restart) | `TestCredentialGrantKeyIsSharedAndDomainSeparated` |
| HA-P0-07 | `planehttp` budgets/classification, artifact-record class, request keys | yes (stage clients ride a restart) | `planehttp/retry_test.go` table + CHAOS-6 client cases |
| HA-P0-08 | daemon drain stops at a stage boundary (`cmd/goobers/up.go`) | yes (bounded drain) | `TestDaemonDrainStopsAtStageBoundary` + `TestUpIdlesThenDrainsOnCancel` |
| HA-P1-06/07/08 | ~10–15k lines **moved** out of `package main` into `internal/controlplane/*` | no | `make ci`; complexity re-baseline by symbol; growth gate shows `cmd/goobers` shrinking |
| HA-P1-09 | `localscheduler` / `triggerqueue` consumed through interfaces | no | existing suites + `TestSchedulerPolicyStoreSuite/file` |
| HA-P1-10/11 | journal storage seam (#2051) for the live writer, engine projection and shared read paths; file backend = today's code path. The local runner keeps its direct file journal | no | goldens, digests, `TestJournalBackendConformance/file`, parity rows |
| HA-P1-12 | read-model callers on `readmodel.Backend` | no | `readmodel/conformance` |
| HA-P1-14 *(fixtures only)* | equivalence corpus recording local journals (test code) | no | — |
| HA-P2-03/04 | `goobers worker` health endpoint; config by digest; emptyDir work root | `goobers worker` only | worker tests |
| HA-P2-05 | re-attach in `internal/engine` + `internal/dispatcher` | engine histories gain a version marker; `goobers worker` behaviour improves | replay tests over captured histories |
| HA-P2-07 | worker versioning automation (`internal/workerhost`) | `goobers worker` only, opt-in | routing tests |
| HA-P3-07 | `engine.TemporalStarter` adds a start-owner memo; the "assigned run ID required" guard is cloud-wired only | local: memo only | starter tests |
| HA-P3-10 | rollup ingest cursor and export catch-up behind a `(stream, seq)` interface; local keeps byte offsets | no | telemetry suites |
| HA-P3-12 | `readmodel` feed behind an interface | no | feed and SSE tests |
| HA-P3-13 | `configgeneration` and `SurrenderPlane` behind interfaces over today's file stores | no | config-generation and surrender conformance (file) |
| HA-P3-14 | interfaces in `internal/recovery`, `internal/cancelreceipt`, `internal/terminalreport` | no | conformance suites (file) |
| HA-P3-15 | telemetry plane handler behind an interface | no | `TestDefectNominationAggregatesParity` (file leg) |
| HA-P3-16 | stall-sweep logic in a shared package with an injected activity clock and downtime source | no | `stalledruns_test.go`, `stalledengineruns_test.go` |
| HA-P3-17 | pod-token `kid` + {current, previous} verify + reload (`internal/podauth`) | tokens carry `kid`; single-key installs unchanged | podauth suites; old-token acceptance test |
| HA-P3-26 | mode-3 deprecation notice | warning text only | help golden |
| HA-P4-01 | runtime-support column + `RUNTIME001`–`003` | **new info line** in local `goobers validate` | validate goldens updated deliberately |
| HA-P4-02 | `goobers validate --target cloud` | new flag only | readiness-check tests and goldens |
| HA-P4-04…16 | engine walk, `__dispatch-exec` supervisor (outbox, usage), surrender contract (additive), bandit store seam | engine-driven runs gain features; the local runner is untouched | parity rows, replay, recovery tests |
| HA-P5-03 | additive in-memory export/import of scrub-registry entries (`internal/journal/scrub.go`); routing middleware in the shared planes, inert without run owners | no | scrub suites; planes suites |
| HA-P5-04 | terminal hooks in `internal/controlplane/enginerun` take an effect-intent recorder (no-op locally) | no | hook tests |
| HA-P5-05 | provider-observation seam in `localscheduler` (in-memory locally) | no | quota and open-PR suites |
| HA-P6-01 | **removal** of mode-3 control-plane commands from `goobers` (`up` with remote runners, `worker`, `engine-*`) | yes, after one deprecated minor | supportpolicy windows; help/man goldens |
| HA-P6-02 *(conditional)* | terminal-intent activity in the Run workflow under `GetVersion` | engine histories gain a marker | replay tests |

That is **45 issues that change the binary**, plus 3 that change local test fixtures only. Shared `go.mod`: new modules must not change any module version in `goobers`' closure; HA-P1-01 makes that a checklist item and `TestGoobersClosureExcludesCloudDeps` asserts the forbidden set.

### 9.2 Mode 3's fate inside `goobers` [APPROVED, D-3]

- **Stays in `goobers` forever:** `__dispatch-exec` and the stage contract (stage pods run the `goobers` binary, dsl-3.0 D6).
- **Unchanged until P3 exit:** mode 3 in `goobers` keeps today's behaviour, including the runner fallback for lanes the engine refuses (`RUNTIME001`/`002` are warnings there).
- **Frozen at P3 exit:** the mode-3 control plane (`goobers up` with an `engine:` and non-self runners, `goobers worker`, `engine-*`) gets fixes only, plus a deprecation warning pointing to `goobers-cloud`. `deploy/reference/goobers-system` stays as the **mode-2** base; cloud gets `deploy/reference/goobers-cloud`.
- **Removed in P6**, after one released minor in `deprecated` (the supportpolicy windows apply). Engine and worker *packages* stay in `internal/`, imported by `goobers-cloud`.
- Mode 2 is frozen: it gets the P0 fixes only.

### 9.3 Import rules [APPROVED] (HA-P1-04, HA-P3-18)

`test/architectureimports` gains closure rules:
- `go list -deps ./cmd/goobers` must not contain `github.com/jackc/pgx/...`, `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/...`, any S3 SDK, or `github.com/goobers/goobers/internal/cloud/...`. The same applies to the local-only and shared packages `internal/runner`, `internal/localscheduler`, `internal/journal`, `internal/livejournal`, `internal/readmodel`, `internal/triggerqueue`.
- `go list -deps ./cmd/goobers-cloud` must not contain `internal/runner` (D-2: the cloud control plane never runs the local runner). Shared helpers that live in `internal/runner` today (for example the bandit `AssignAndRecord`) move to shared packages in the P4 item that needs them.

Cloud implementations live under `internal/cloud/*` and are injected into shared code only through interfaces defined in the shared packages. **This is what makes "local is not linked against Postgres" true and enforced.**

### 9.4 Local↔cloud journal equivalence [APPROVED] (HA-P1-14, HA-P3-24)

- **Corpus.** A fixed corpus of deterministic DSL 3.x workflows covering every emit-plane event kind the engine supports: retries (policy, infra), automated and agentic gates, artifacts, inputs, HITL markers, cancellation. Each P4 item adds entries for its feature (parallels, selection, maps, children, human gates, outbox, limits, experiment).
- **Assertion.** Run (a) by the local runner on the file backend and (b) by `goobers-cloud` on the engine and PG backend. Canonical journals (scrubbed event bytes, ignoring only timestamps, run ID, host identity and durations), artifact digests and the A2 conformance projection must be equal.
- **Where it runs.** The local leg runs in required shards (P1). The cloud leg runs in `cloud-pg` (P3). It compares cloud to **local**, which is also the runner↔engine parity check end to end.

### 9.5 Features that diverge between local and cloud

| Feature | Local (`goobers`) | Cloud P3–P5 | Cloud after P4 exit |
|---|---|---|---|
| DSL 2.0 workflows | supported (runner) | refused (`RUNTIME003`) | refused (dsl-3.0 D14) |
| The 8 R9 features | supported (runner) | refused until the feature's P4 item ships (`RUNTIME001`) | supported (engine) |
| `self` placement, instance-root-only stages (for example `reconcile-branches`) | supported | refused (`RUNTIME002`) unless migrated to a remote stage | same |
| Pinned-workspace (large-repo) custody | supported | unsupported | unsupported (non-goal) |
| `telemetry.db` CLI analytics | full | minimal port (D-8) | same |
| HITL resume/intervene | any time on the runner; engine runs within `engine.hitl.window` | engine; window ≤7d (D-9) | same |
| `maxRunDuration` / stall | [§10.3](#stall-semantics) | §10.3 | §10.3 |
| Run-ID reuse | [§10.1](#run-id-semantics) | identical | identical |
| CLI | instance-root commands | `goobers-cloud` + `--api` operator commands (HA-P3-19) | same |

---

<a id="dsl-semantics"></a>
## 10. DSL-visible behaviour: exact semantics

No workflow, goober or gaggle schema change and no new `dslVersion` are needed. Cloud configuration (DB, object store, lease, scrub) lives in a new `goobers-cloud` config file, not in the DSL.

<a id="run-id-semantics"></a>
### 10.1 Run IDs and Idempotency-Key reuse [APPROVED] (HA-P3-07)

- **Rule.** In cloud, **every run start is backed by a durable acceptance row**, and `run_id` is that row's random 128-bit ID (lowercase hex, `dispatchidentity.go` format). This is today's queued-path mechanism, made mandatory.
- **Direct paths** get an acceptance row first: `run continue` becomes an API trigger with an `Idempotency-Key`; child admission attempts are keyed by the child acceptance row; scheduler dispatches with an empty assigned ID are rejected by a cloud-only guard.
- **Reuse semantics.** An `Idempotency-Key` dedups for as long as its acceptance row is retained: 7 days after finish (`triggerqueue/store.go:28`). After that, reusing the key **starts a new run with a new ID and a new branch name**, locally and in cloud alike. Temporal's `REJECT_DUPLICATE` never sees an old ID, because IDs are random per acceptance.
- **Docs:** `docs/reference/queued-scheduled-starts.md` gets one sentence saying cloud has the same 7-day replay window.
- **Why not hash an occurrence key.** Hashing the client key would return the old closed run after 7 days (`starter.go:138-140`) and land on the old branch. The acceptance row already *is* the occurrence.

<a id="occurrence-keys"></a>
### 10.2 Occurrence key per trigger kind [EXISTING keys, ported]

| Trigger kind | Dedup key that creates the acceptance row [EXISTING] | Exactly-once unit |
|---|---|---|
| API trigger (`POST /triggers`, `run continue` in cloud) | `Idempotency-Key` = PK, actor + payload fingerprint (`store.go:131-180`) | one acceptance per key within retention |
| GitHub webhook | `source_key = sha256("github-webhook", X-GitHub-Delivery)` + recipient ordinal (`source_start.go:14-17,103-111`) | one acceptance per delivery × recipient |
| Cron / schedule | `source_key = H("schedule", scope(gaggle, workflow), revision, before, after)` for the coalesced window, written **in the same transaction** as the cursor CAS `WHERE cursor_ns = before` (`startintent/schedules.go:20-44`); per-declaration occurrences use the 1-based `Occurrence` index when not coalesced (`source_start.go:28-30`) | one acceptance per cursor advance; there is no "fire ordinal"; a second scheduler with a stale `before` fails the CAS |
| Item/backlog workers | `source_key = H("worker-observation", gaggle, workflow, kind, observedAt)` + worker ordinal 1..Count (`startintent/workers.go:21-53`) | one acceptance per observation × ordinal |
| Demand / refill | demand transfer inside the same source batch (`transferScheduleDemand`), keyed by demand ordinal | one transfer per ordinal |
| Signals / `sourceRun` / priority | signal source receipt keys (`cmd/goobers/signalsource.go:70-100`) | one acceptance per signal receipt |
| Child workflow starts | child acceptance row (`triggerqueue/children.go:218`) | one per child proposal |
| Fan-out (≤32 starts per receipt, `source_start.go:20`) | one acceptance row per start in the batch, atomically | all-or-nothing batch |

**Leader fencing on top.** Each acceptance's `accepted → dispatching` transition is a fenced write stamping `owner_epoch` ([§11.2](#lease)). The uncertain set after a leadership change is "`dispatching` rows whose `owner_epoch` ≠ the live epoch", which replaces the boot-time `UncertainIDs` snapshot.

<a id="stall-semantics"></a>
### 10.3 `maxRunDuration` and `stalledRunTimeout` in the controller [APPROVED] (HA-P3-16)

Today the daemon's stalled sweep subtracts host suspensions and drained downtime (`cmd/goobers/stalledruns.go:24-59`), and liveness comes from in-memory `LastActivity` (`livejournal.go:553-567`).

| | Local (unchanged) | Cloud |
|---|---|---|
| Activity clock | in-memory `LastActivity` on the live writer | `journal.run_seq.last_append_at`, set to DB `now()` in every committed emit transaction: same meaning ("last journal append"), durable, readable by any controller |
| `stalledRunTimeout` | silence on the live journal ⇒ escalate | `now() − last_append_at > stalledRunTimeout` ⇒ escalate (same journal event, same escalation path) |
| `maxRunDuration` = active execution time | run age − host suspensions − durable child waits − drained downtime after the last activity | run age − durable child waits. **Nothing else is subtracted:** every cloud run is an engine run whose stages keep executing in pods while the controller restarts, and host suspension does not exist in cloud |
| Enforcement | sweep terminalizes; engine runs Cancel then Terminate when no worker serves (#5407) | same sweep logic, moved to a shared package with an injected clock and downtime source, run by L2 on the lease holder |

**Docs change:** one paragraph in `docs/reference/workflow-primitives/graph-and-execution.md` (the `maxRunDuration` row and the "requires daemon-backed execution" note at `:441`): cloud is controller-backed, and neither host suspension nor controller downtime is credited.

<a id="reattach-semantics"></a>
### 10.4 Re-attach vs per-attempt `timeoutSeconds` [APPROVED] (HA-P2-05)

Today Temporal runs every stage activity with `MaximumAttempts: 1`, and StartToClose is the stage `maxDuration` + grace (`internal/engine/engine.go:1349-1360`). Attempts map 1:1 to journal attempts (`retry.go:105-117`).

1. **A worker handoff is not an attempt.** On SIGTERM the activity returns `worker-handoff`. Under `GetVersion("dispatch-reattach-v1")`, the walk re-schedules the **same attempt number**, charges neither the policy nor the infra budget, and emits no new `stage.started`.
2. **The attempt deadline is fixed at first start:** `deadline = attempt_started_at (journaled) + timeoutSeconds`. The re-scheduled activity's StartToClose is `deadline − now + stageTimeoutGrace`, never a fresh window. If `deadline − now ≤ 0`, the attempt is classified `timeout`, exactly as without the handoff. The adopted pod's `activeDeadlineSeconds` is unchanged.
3. **Adoption.** The retried activity reads the last heartbeat details `{namespace, pod, uid, attempt, custody}`: pod alive with matching uid and attempt ⇒ adopt and watch to surrender; pod gone without a surrender ⇒ an **infra** attempt, as today.
4. **Side-effecting stages** are no longer failed by a handoff, since nothing re-executes. They are still never retried after an *unrecoverable* worker loss (`retry.go:286-288` is unchanged for that case).
5. **Handoff budget.** At most 5 handoffs per attempt (`handoff.exhausted` ⇒ infra), so a crash-looping worker fleet can't hold an attempt forever.

**Docs change:** the retry/timeout reference gets a short section: "a worker handoff does not consume an attempt and does not reset the attempt's timeout clock".

### 10.5 HITL hold vs worker version cap [APPROVED] (HA-P2-07, HA-P5-09)

- Worker Deployments keep at most **3 live versions**; each is a full Deployment of at least 2 pods, so 6 or more idle pods at the cap, and images for all 3 must stay pullable.
- Cloud caps `engine.hitl.window` at 7d (D-9).
- A run pinned to a retired version gets a versioning override to **AUTO_UPGRADE**; its next workflow task replays on the current build. **`GetVersion` patching discipline therefore stays mandatory** for every workflow change. Pinning buys rollout smoothness, not freedom from patching.
- Gate: an `UPGRADE-HITL` replay test resumes, on build N, a hold started on build N−3.

---

<a id="mechanisms"></a>
## 11. Mechanisms

<a id="start-fence"></a>
### 11.1 Start fence: `AlreadyStarted` must mean "not mine" for a zombie [APPROVED] (HA-P3-07)

**Fact [EXISTING].** `TemporalStarter.start` maps `WorkflowExecutionAlreadyStarted` to `StartResult{AlreadyRunning: true}, nil` (`internal/engine/starter.go:138-140`). A zombie leader's duplicate start therefore *succeeds* from the caller's point of view.

**Fix:**
1. **Start-owner memo.** Every cloud start carries `Memo["goobers.start-owner"] = {acceptance_id, owner_epoch, starter_id}`, next to the existing `RunInputDigestMemoKey` (`starter.go:133-135`).
2. **On `AlreadyStarted`**, the starter calls `DescribeWorkflowExecution(run_id)` and compares the memo:
   - same `acceptance_id` and `owner_epoch` ⇒ *mine* (a retry of my own start whose response was lost); proceed idempotently, every following write is keyed;
   - same `acceptance_id`, older `owner_epoch`, seen by the **current** leader during reconciliation ⇒ *adopt*: a fenced write moves the acceptance row to `dispatched, owner_epoch = mine`; the slot is rebuilt from open workflows (rebuild-before-reap); the predecessor's reservation emit dedups by key;
   - any other case, including a **newer** `owner_epoch` ⇒ **not mine**: no claim write, no slot accounting, no `run.started` emit, no terminal-hook registration; the caller re-validates its lease and demotes if stale (`StartResult{NotMine: true}`);
   - missing or malformed memo ⇒ refuse; absent evidence never proves ownership.
3. **Fenced acceptance transition.** `accepted → dispatching` is a fenced write stamping `owner_epoch`, so a zombie with a stale epoch can't reach step 2 at all. The memo check covers the window in which the zombie passed the fence just before losing it.
4. **Cloud-only after #7783.** Local `goobers` no longer starts engine workflows. `cmd/goobers-cloud` supplies a durable-admission starter factory, lease binding, recovery and status seams. HA-P3-18 (#7818) connects them to the controller loop; continue/child admission entrypoints remain part of that wiring.
5. **Two-phase custody.** `BeginDispatch` is the provisional reservation. Durable `run_id` and the live handle supply start authority before Temporal. Only mine/adopt confirms `dispatched`; adoption compares the exact predecessor epoch observed before Temporal. NotMine leaves ownership unchanged, even if lease revalidation also fails.
6. **Quarantine and recovery.** Dispatches uncertain for one minute appear as `start ownership uncertain`. A fenced sweep reserves at most 32 retry attempts using `SKIP LOCKED`, with four concurrent attempts, ten-second attempt deadlines, and durable exponential backoff capped at one minute beyond the attempt deadline. A failed attempt leaves its row dispatching; other runs proceed. Status pages contain identity and retry time, never pinned payloads.

Tests: `TestZombieStartIsNotMine` (two starters, epochs E and E+1, one acceptance row ⇒ exactly one claim row, one slot, one `run.started`), `TestLostStartResponseIsMine`, `TestReconcileAdoptsPredecessorStart`; CHAOS-3 end to end.

<a id="lease"></a>
### 11.2 Lease and fence [APPROVED] (HA-P3-03, HA-P3-04)

The lease and its heartbeat are **separate rows**, so renew never conflicts with the leader's own fenced transactions.

```sql
-- authority: changes only on takeover or voluntary release
CREATE TABLE control.lease (instance_id text, name text, holder text NOT NULL, epoch bigint NOT NULL,
  PRIMARY KEY (instance_id, name));                       -- name: 'controller','projector','gc','migrate'
-- liveness: touched every renew; never locked by fenced transactions
CREATE TABLE control.lease_heartbeat (instance_id text, name text, holder text NOT NULL, epoch bigint NOT NULL,
  expires_at timestamptz NOT NULL, PRIMARY KEY (instance_id, name)) WITH (fillfactor = 50);
CREATE TABLE control.lease_history (instance_id text, name text, epoch bigint, holder text,
  acquired_at timestamptz, released_at timestamptz, release_kind text,      -- 'voluntary' | 'expired'
  PRIMARY KEY (instance_id, name, epoch));                                  -- CHAOS assertions and audit

-- renew (every 5s; TTL 15s): touches ONLY the heartbeat row
UPDATE control.lease_heartbeat SET expires_at = now() + interval '15 seconds'
 WHERE instance_id=$i AND name=$n AND holder=$me AND epoch=$E RETURNING expires_at;   -- 0 rows ⇒ demote

-- takeover (standby every 2s; the single P3 pod at boot)
BEGIN;
SELECT expires_at FROM control.lease_heartbeat WHERE instance_id=$i AND name=$n FOR UPDATE;  -- serializes vs renew
-- if expires_at >= now() AND holder <> $me ⇒ ROLLBACK (still held)
UPDATE control.lease SET holder=$me, epoch=epoch+1 WHERE instance_id=$i AND name=$n RETURNING epoch;
   -- blocks until in-flight fenced transactions (FOR SHARE on control.lease) commit or abort
UPDATE control.lease_heartbeat SET holder=$me, epoch=$new, expires_at=now()+interval '15 seconds' WHERE …;
INSERT INTO control.lease_history …;
COMMIT;

-- fenced write: every leader-originated transaction
BEGIN;
SET LOCAL statement_timeout = '2s'; SET LOCAL idle_in_transaction_session_timeout = '2s';
SELECT 1 FROM control.lease WHERE instance_id=$i AND name='controller' AND holder=$me AND epoch=$E FOR SHARE;  -- 0 rows ⇒ ROLLBACK, demote
… writes, each stamping epoch/owner_epoch where the table has the column …
COMMIT;
```

- **Why renew can't self-block.** Renew updates `lease_heartbeat`; fenced transactions share-lock `lease`; only takeover touches both, and takeover *should* wait for in-flight fenced transactions.
- **Bounded fenced transactions:** 2s statement and idle timeouts. Long work (rebuild-before-reap scans, GC batches, config apply) is chunked into fenced transactions of at most 2s each, with unfenced reads between.
- **MultiXact pressure:** fenced writes use a dedicated pool of ≤4 connections, so at most 4 concurrent share-lockers hold the hot row.
- **Self-fence:** the leader stops L1/L2/L3 when monotonic time since its last successful renew exceeds 10s (TTL − 5s skew margin). All expiries use DB `now()`, never pod clocks.
- **Voluntary release on SIGTERM** bumps the epoch, so a clean handover takes ≤5s.
- **Effects outside the DB:** Temporal starts use the start fence (§11.1); blobs are content-addressed; provider mutations from the controller (labels, comments, branch ops) go through **fenced intents with readback** (HA-P5-04): a fenced `INSERT control.effect_intent(key, generation, epoch)`, then the effect, then a readback compare-and-set where the provider supports it. Residual duplicates are bounded by the self-fence window.
- **Write-path inventory (HA-P3-04).** A checked-in table lists table × writer role × fence kind (`epoch` | `owner_token` | `owner_epoch` | `unique key` | `none-by-design`). `TestColumnGrantsMatchWritePathInventory` asserts the PG column-level `GRANT`s match it, so a future api handler writing `control.acceptance.state='dispatching'` fails CI.
- **No k8s Lease** ([§21](#alternatives)).

**DB invariants, independent of leadership:** `control.acceptance` keys per trigger kind ([§10.2](#occurrence-keys)) + cursor CAS; `control.claim` PK `(instance_id, gaggle, provider, external_id)` with owner token, epoch and DB-clock expiry; `journal.run_seq` counter + `UNIQUE (instance_id, run_id, emit_key)`; `control.surrender` write-once.

<a id="journal-write-path"></a>
### 11.3 Journal write path [APPROVED] (HA-P3-09)

**P1 storage seam (HA-P1-10).** Shared writers target
`internal/journal.Backend`: create/recover returns an opaque handle plus owner
token, and every
append batch, idempotent append, notification-delivery claim, checkpoint, blob
or span record, child-start bind, transcript write and retirement presents that
token. Instance annotations use an independently fenced instance handle. A file
append batch has **ordered-prefix** durability: each event is independently
fsynced and checkpointed in argument order; after an error or crash, recovery
may expose a possibly empty prefix, never a reordered batch or a later event
without its predecessors. Validation failures commit nothing. `AppendBatch`
returns the durable prefix length so callers resume a retry after that prefix.
`FileBackend` delegates to today's `Run` and `InstanceLog`, preserving
the existing bytes, scrub-before-digest rule, fsync boundaries, atomic rename
and lifetime flock. The local runner remains directly file-bound; only
`livejournal` and the Temporal history projection use the seam. The later PG
backend replaces the owner token with the fenced run-owner epoch and preserves
the same conformance suite.

**P1 read seam (HA-P1-11).** Shared read paths target
`internal/journal.ReaderBackend`: bounded event reads after a sequence, filtered
run listing, incremental instance-log reads, and typed run handles for
identity/checkpoint/blob access. `FileReaderBackend` preserves schema migration,
torn-tail tolerance, digest verification and lexical discovery. Readservice,
the read-model projector and its repair sweep use the seam; CLI-only
instance-root readers remain directly file-bound.

**P1 read-model callers (HA-P1-12).** The SSE router depends on
`readmodel.Backend` and the narrow `readmodel.Feed` read capability, never on
the SQLite `*readmodel.Store`. The projector reaches run evidence through the
journal read seam above. Backend conformance includes definition changes in the
same dense ordered feed as projected run changes.

```sql
CREATE TABLE journal.run (instance_id text, run_id text, gaggle text NOT NULL, workflow text NOT NULL,
  run_yaml bytea NOT NULL, created_at timestamptz NOT NULL, retired_at timestamptz,
  PRIMARY KEY (instance_id, run_id));                                   -- wide, written rarely
CREATE TABLE journal.run_seq (instance_id text, run_id text, last_seq bigint NOT NULL DEFAULT 0,
  owner_epoch bigint NOT NULL DEFAULT 0, last_append_at timestamptz NOT NULL,
  PRIMARY KEY (instance_id, run_id)) WITH (fillfactor = 70);           -- narrow, HOT-updatable
CREATE TABLE journal.run_checkpoint (instance_id text, run_id text, phase text NOT NULL, terminal boolean NOT NULL,
  state bytea, at_seq bigint NOT NULL, PRIMARY KEY (instance_id, run_id));   -- updated on phase change only
CREATE TABLE journal.run_event (instance_id text, run_id text, seq bigint, branch int NOT NULL DEFAULT 0,
  type text NOT NULL, stage text, attempt int, emit_key text, at timestamptz NOT NULL,
  body bytea NOT NULL,                                                   -- exact scrubbed line; digests commit to bytes
  PRIMARY KEY (instance_id, run_id, seq));
CREATE UNIQUE INDEX ON journal.run_event (instance_id, run_id, emit_key) WHERE emit_key IS NOT NULL;
CREATE TABLE journal.run_artifact (instance_id text, run_id text, seq bigint, digest text NOT NULL,
  kind text, PRIMARY KEY (instance_id, run_id, seq, digest));
CREATE INDEX ON journal.run_artifact (digest);                           -- GC reachability (DS8)
CREATE TABLE journal.instance_seq (instance_id text PRIMARY KEY, last_seq bigint NOT NULL DEFAULT 0);
CREATE TABLE journal.instance_event (instance_id text, seq bigint, at timestamptz NOT NULL, type text,
  run_id text, emit_key text, epoch bigint, body bytea NOT NULL, PRIMARY KEY (instance_id, seq, at))
  PARTITION BY RANGE (at);                                               -- monthly partitions
```

**Counter rows, not `SEQUENCE`.** Sequences are allocated before commit and aren't gapless, so a tailing reader could skip a lower seq that commits later. A counter row inside the insert transaction gives gapless, commit-ordered seqs. DS4's "single writer owns sequence allocation" becomes "the database owns sequence allocation" ([§20](#departures)).

Emit transaction (the `control` pod in P3; the run's owner api replica in P5):
1. **Scrub first, outside any lock**, with the in-memory registry ([§11.4](#scrubber)).
2. `BEGIN`; 2s statement timeout; dedup lookup on `(instance, run, emit_key)`; on a hit, `COMMIT` and return the original seq.
3. `UPDATE journal.run_seq SET last_seq = last_seq + $n, last_append_at = now() WHERE … AND owner_epoch = $oe RETURNING last_seq`. Zero rows ⇒ fenced out (stale run owner) ⇒ 409 `not_owner`, which the client treats as refused-early.
4. `INSERT` events, artifact pointers and the read-model intake upsert. `run_checkpoint` is touched only when the phase changes; today's `state.json` is derived (`journal/doc.go:11`) and is rebuilt on read.
5. `COMMIT`, then 2xx. **There is no `NOTIFY` in this transaction.**

Live tail:
- After commit, each replica's **notifier** coalesces "run R reached seq S" and issues at most one `NOTIFY run_events` per 250 ms, on a dedicated non-pooled connection, outside any write transaction. The payload lists run IDs (≤8000 bytes), or `*` on overflow.
- Readers also poll `(run_id, seq > cursor)` every 1s, so NOTIFY is only a latency optimization and adds no database-wide commit serialization.
- **Global instance counter.** `journal.instance_seq` is one row. A property test measures it under a 32-start fan-out burst plus 3 emitters; if the p99 lock wait exceeds 20 ms, instance annotations are sharded per gaggle stream (a reader change, `(stream, seq)`). Leader-originated instance events go through the fenced-write helper and stamp `epoch`; instance-annotation dedup (memory-only today) becomes a unique `emit_key` per partition.
- **Throughput budget.** Per-run appends serialize on one narrow row: about 2 RTT + commit, with scrub outside. Transcript emitters already batch (`AppendBatch`). A 500 emits/s per-run soak is **reported** nightly, never asserted in required CI.

<a id="scrubber"></a>
### 11.4 Redaction: byte-identical, per-run writer affinity [APPROVED; D-5 default adopted]

**Today [EXISTING]:**
- `RegistryScrubber` holds raw values and redacts up to **three** forms per secret: raw, JSON-escaped with HTML escaping, and JSON-escaped without. It skips values under 6 bytes, sorts forms longest-first with byte-order tie-breaks, and applies sequential `bytes.ReplaceAll` (`internal/journal/scrub.go:72-200`).
- It is fed at mint time by the issuing process (`cmd/goobers/credentialrefresh.go:163,193`; `internal/githubapp/source.go:239`; `internal/credentials/capability.go:41`; `internal/childworkflow/grant_issuer.go:84`).
- Digests commit to the scrubbed bytes (`scrub.go:72-77`), and conformance compares them. Pattern scrubbers are the backstop.

**Why no shared registry in the database.** Raw or encrypted secrets at rest in the DB (and its backups) are rejected outright. A registry of keyed fingerprints (`HMAC(k_scrub, form)`) was evaluated and rejected:
1. **Tie order.** Output depends on byte-order tie-breaking among equal-length forms; a fingerprint registry can't order forms by their bytes, so byte identity with today is unachievable without changing the canonical algorithm in *both* binaries.
2. **Prefilter oracle.** A fast scan needs either a stored per-secret rolling hash (an oracle without the key) or a scheme that still scans every window.
3. **Availability coupling.** "Commit the fingerprint before returning the credential" makes credential minting fail during a DB failover and changes four shared `Register` signatures.

**Decision:**
- **P3:** one writer process (`control`). Journal emits, credential minting and the scrubber are co-located, exactly as today. Output is byte-identical by construction.
- **P5: per-run writer affinity.**
  - **Owner.** Run R has an owner api replica, recorded in `control.run_owner(instance_id, run_id, holder, epoch, lease_until)` on the DB clock. The preferred owner is a rendezvous hash of `run_id` over live replicas (`control.replica` heartbeat rows).
  - **Routing.** The run-scoped planes (journal emit, credential resolve/refresh, surrender, artifact records) for R are served by R's owner; any other replica proxies one hop to the owner's pod IP over the headless Service with an internal service bearer. Owner-epoch fencing ([§11.3](#journal-write-path)) rejects stale owners.
  - **Worker emits.** Bytes from engine walks arrive through the emit plane and are scrubbed by the owner. Workers keep their own `DefaultScrubber` pre-scrub, as in mode 3 today.
  - **Graceful handoff.** On SIGTERM the owner transfers each owned run's in-memory registry entries (values + expiries) **in memory** to the run's next rendezvous owner over the authenticated internal channel, then releases its `run_owner` leases. Nothing is persisted. Rolling updates have **no** redaction gap.
  - **Crash.** The successor acquires epoch+1 without the dead owner's registry entries. Credentials it minted, still live in pods, are redacted only by pattern scrubbers until they expire (TTL ≤1h for minted tokens). **This is exactly today's daemon-restart gap.** HA-P5-03 measures how many registered values in the corpus pattern scrubbers miss.
  - Benchmarks are reported nightly, never a required wall-clock gate.
- **HA-P6-03.** A shared registry is reconsidered only if the measured crash gap is material **and** the PO approves changing the canonical algorithm in both binaries, with differential fuzzing against `RegistryScrubber`. Otherwise it closes as "won't do".

<a id="client-retry"></a>
### 11.5 Plane client retries [APPROVED] (HA-P0-07)

| Item | Today [EXISTING] | Change |
|---|---|---|
| Budget | 45s wall **and** 8 tries (≈9–18s real backoff; `planehttp/retry.go:42-48`) | **Time-based only:** `budget = min(240s, remaining stage deadline − 15s)`. No try cap. Backoff 250 ms → 5s with full jitter |
| Retry-After | capped at 5s | capped at 15s, never past the budget |
| Bare 502 / 503 / 504 (no Goobers error envelope) | permanent | **ambiguous** (the proxy may have reached a handler that committed). Retried **only** for reads and replay-safe/keyed mutations. Every plane stage pods use is replay-safe: journal emit (key), claims acquire/renew/release (semantic), triggers (key), surrender (identity), blobs (digest); `release-with-report` and `verify` get request keys |
| Goobers 503 `recovering` / `class_saturated`, 429 `class_saturated`, dial failure, `REFUSED_STREAM` | refused-early | unchanged (refused-early, any method) |
| Read timeout after the request was sent | permanent | ambiguous |
| DNS NXDOMAIN, TLS verification failure, 401/403/404 | permanent | permanent, **fail fast** (a broken NetworkPolicy or deleted Service must not cost 4 min per call) |
| Exhaustion | caller-dependent | **always `infra`**, even when the stage deadline fired during a plane retry (wraps `planehttp.ErrBudgetExhausted`, mapped to `infra` before timeout) |
| Artifact records (`dispatchexec.go:1300-1301`) | 3s, log-only | plane class (same budget) |
| Journal emit / blob clients | 60s | same budget rule |
| Credential resolve | 3 min | unchanged |

Tests: `TestPlaneRetryBudgetIsTimeBased`, `TestClassifyBareProxy5xxAmbiguous`, `TestPlaneExhaustionIsInfraEvenAtStageDeadline`, `TestNXDOMAINFailsFast`, `TestReleaseWithReportReplaySafeWithKey`, plus CHAOS-6 client-side cases against today's singleton.

---

<a id="data"></a>
## 12. Data and state

### 12.1 Store mapping [APPROVED]

One database per instance (`goobers_<instance>`) on a shared server, with schemas `journal`, `control` and `readmodel`. Every key includes `instance_id` so a later shared-database layout stays possible.

| Today (file / SQLite) | Cloud target | Notes |
|---|---|---|
| `runs/<id>/events.jsonl`, `state.json`, `run.yaml` | `journal.run`, `journal.run_seq`, `journal.run_checkpoint`, `journal.run_event`, `journal.run_artifact` | `body bytea` = exact scrubbed line; JSONB only as a non-authoritative generated column |
| `scheduler/events.jsonl[.gen-N]` | `journal.instance_event` (monthly partitions) + `journal.instance_seq` | consumers re-key from byte offset to `(stream, seq)` (rollup ingest cursor `telemetry/rollup/schema.go:406-410`, export catch-up `journalcatchup_store.go:20-27`); local keeps offsets behind the same interface |
| `health/events.jsonl` | `journal.health_event` | |
| `claims.json` + history | `control.claim`, `control.claim_history` (30-day purge) | owner token + epoch + DB-clock expiry (sharedclaim-shaped); merge lock = a renewed claim row that fails closed |
| scheduler-state keys, M5 files, watermarks | `control.scheduler_state(scope, key, value, etag, owner_epoch)` | same transaction domain as claims via `pg_advisory_xact_lock('claims:'||instance)` (PgBouncer-safe; preserves today's coarse serialization) |
| `accepted-triggers.db` (acceptances, source receipts, schedule cursor, demand, child custody) | `control.acceptance` (`id = trigger-<runid>`, `owner_epoch` on `dispatching`), `control.source_receipt`, `control.source_cursor`, `control.schedule_demand`, child custody tables | the cursor CAS + source receipt is the double-fire invariant |
| `cancellation-receipts.db` | `control.cancel_receipt` | |
| terminal-report intents (#7671) | `control.terminal_report` (DB-clock leases, generation, `SKIP LOCKED`) | P3 |
| engine terminal hooks | `control.effect_intent` (fenced, P5); full `control.effect_outbox` only if HA-P6-02's criterion holds | |
| surrender docs | `control.surrender` | write-once; a differing body ⇒ 409 |
| config dir + generations | object-store archives by digest; `control.config_generation`, pins, `control.applied_config` | monotonic CAS under the fence; followers load by digest |
| recovery records | bundle → object store; `control.recovery_record` | identity immutable; `retain_until` monotone (trigger) |
| `read.db` + `intake.db` | schema `readmodel_v<N>` (intake joins the projection transaction) | projector under the `projector` lease; version-and-rebuild, then pointer flip |
| `telemetry.db` | `telemetry` schema for `first_success_milestones` + 4 defect-nomination aggregates; the rest to OTLP (D-8) | |
| blobstore PVC | object storage | GC ([§12.3](#blobs-gc)) |
| `up.lock` + mtime heartbeat | `control.lease` + `control.lease_heartbeat` + `control.lease_history` | |
| provider quota, MaxOpenPRs (memory) | leader memory + `control.provider_observation` | a new leader starts warm (P5) |
| credential grant revocations (memory) | `control.grant_revocation(grant_id, expires_at)` | |
| (new, P5) | `control.run_owner`, `control.replica` | |
| (new, P4) | `control.runner_bandit` | experiment observations |
| git mirrors, worktrees, read caches, harness home | node-local emptyDir | caches only |
| pinned-workspace custody | **unsupported in cloud** | |

### 12.2 Postgres schema sketch, `control` (abridged) [PROPOSED]

```sql
CREATE TABLE control.acceptance (instance_id text, id text, kind text NOT NULL, idem_key text,
  actor text, fingerprint bytea NOT NULL, payload bytea,
  state text NOT NULL CHECK (state IN ('accepted','dispatching','dispatched','rejected')),
  owner_epoch bigint, run_id text NOT NULL, accepted_at timestamptz NOT NULL, finished_at timestamptz,
  PRIMARY KEY (instance_id, id), UNIQUE (instance_id, run_id));
CREATE UNIQUE INDEX ON control.acceptance (instance_id, idem_key) WHERE idem_key IS NOT NULL;
CREATE INDEX ON control.acceptance (instance_id, accepted_at) WHERE state = 'accepted';
CREATE TABLE control.source_receipt (instance_id text, source_key bytea, ordinal int,
  acceptance_id text NOT NULL, fingerprint bytea NOT NULL, received_at timestamptz NOT NULL,
  PRIMARY KEY (instance_id, source_key, ordinal));
CREATE TABLE control.source_cursor (instance_id text, source text, cursor_ns bigint NOT NULL,
  owner_epoch bigint NOT NULL, PRIMARY KEY (instance_id, source));   -- CAS WHERE cursor_ns = $before

CREATE TABLE control.claim (instance_id text, gaggle text, provider text, external_id text,
  run_id text NOT NULL, workflow text NOT NULL, owner_token uuid NOT NULL, epoch bigint NOT NULL,
  claimed_at timestamptz NOT NULL, renewed_at timestamptz NOT NULL, expires_at timestamptz NOT NULL,
  release_pending boolean NOT NULL DEFAULT false, verification jsonb, version bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (instance_id, gaggle, provider, external_id));
-- acquire: INSERT … ON CONFLICT DO UPDATE … WHERE claim.run_id = excluded.run_id OR claim.expires_at < now()

CREATE TABLE control.scheduler_state (instance_id text, gaggle text NOT NULL DEFAULT '', key text,
  value bytea NOT NULL CHECK (octet_length(value) <= 8388608), etag uuid NOT NULL,
  owner_epoch bigint, updated_at timestamptz NOT NULL, PRIMARY KEY (instance_id, gaggle, key));

CREATE TABLE control.surrender (instance_id text, run_id text, stage text, attempt int,
  doc bytea NOT NULL, recovery_ack boolean NOT NULL DEFAULT false,
  PRIMARY KEY (instance_id, run_id, stage, attempt));
CREATE TABLE control.effect_intent (instance_id text, key bytea, generation bigint NOT NULL,
  epoch bigint NOT NULL, state text NOT NULL, created_at timestamptz NOT NULL,
  PRIMARY KEY (instance_id, key, generation));                        -- P5
CREATE TABLE control.provider_observation (instance_id text, provider text, identity text,
  remaining int, reset_at timestamptz, open_prs jsonb, observed_at timestamptz,
  PRIMARY KEY (instance_id, provider, identity));
CREATE TABLE control.grant_revocation (instance_id text, grant_id text, expires_at timestamptz NOT NULL,
  PRIMARY KEY (instance_id, grant_id));
CREATE TABLE control.schema_migration (version int PRIMARY KEY, kind text CHECK (kind IN ('expand','contract')),
  binary_version text NOT NULL, min_compatible_binary text NOT NULL,
  backup_ref text NOT NULL, applied_at timestamptz NOT NULL);
```

<a id="blobs-gc"></a>
### 12.3 Blobs and GC [APPROVED] (HA-P3-13, HA-P6-04)

- Layout `blobs/sha256/<aa>/<rest>` (same as `blobstore.Dir`) with `If-None-Match: *` create-only puts. Readers verify the digest.
- Write order: blob first, then the row that points at it. A crash in between leaves an orphan for GC, never a dangling pointer.
- `recovery/<record>/snapshot.bundle` is immutable, with soft-delete and versioning on.
- **GC (DS8)** is a leader job under the `gc` lease. A digest is deleted only when **all** hold: no row in `journal.run_artifact`, config generations, recovery records or pending adoptions; older than the TTL baseline; unreferenced for longer than the **PITR retention window**, so a restored DB never points at a deleted blob. The collector must observe the same storage ETag unreferenced in two consecutive sweeps separated by more than the maximum publication latency. Every `Put`, including an idempotent existing-blob Put, takes the mutation lease and advances the ETag; digest-only referencers call `blobstore.TouchReference` before committing. A writer class is excluded fail-closed until its complete write surface is recorded in `control.blob_reference_writer_protocol`. Publishers and collectors therefore serialize on the blob's storage lease; config publishers additionally renew the lease before commit, clear the candidate in that transaction, and touch the blob after commit. The collector stamps and conditionally deletes by ETag while it still owns that storage lease, so loss of its database lease cannot race a successful publisher. The census treats missing relations as referenced, includes child `result_ref`/`workspace_ref` values, and excludes recovery `bundle_key` because that is a separate `recovery/...` location rather than a `blobs/sha256/...` digest. GC is pausable for restores; the configured floor and Blob soft-delete are both at least the 35-day PostgreSQL backup window. Physical deletion remains disabled by default behind `--experimental-physical-blob-gc`.
- Today there is **no** general GC caller; the only `Remove` caller is `internal/configgeneration/store.go:232`.

<a id="migrations"></a>
### 12.4 Migrations and schema compatibility [APPROVED] (HA-P3-02, HA-P6-07)

1. `goobers-cloud migrate apply` (a PreSync Job) takes the `migrate` lease and a session `pg_advisory_lock` on a **direct** 5432 connection (session locks don't work through PgBouncer).
2. **Backup first.** Azure: an on-demand backup where the SKU supports it (UNVERIFIED per SKU/region), otherwise record `now()` + `pg_current_wal_lsn()` as the PITR mark. CNPG: create a `Backup` CR and wait for `completed`. With no recordable `backup_ref`, migration refuses.
3. Apply **expand-only** migrations in the release that introduces them: add tables, columns, indexes; new columns nullable or defaulted; no drop or rename. `CREATE INDEX CONCURRENTLY` runs per partition, then `ALTER INDEX … ATTACH PARTITION` on the parent (CIC is unsupported on a partitioned parent).
4. Record `control.schema_migration(version, kind, binary_version, min_compatible_binary, backup_ref)`.

- **Binaries** refuse to start if `self < min_compatible_binary`, go unready on a newer incompatible schema, and **never migrate at startup**.
- **Contract** steps (drops) ship later. A `contract` step sets `min_compatible_binary` to **N, the first release that stopped reading the old shape**, so N pods stay ready while N+1 rolls. A CI guard requires every migration to be tagged.
- **`readmodel`** uses version-and-rebuild (`readmodel_v<N>` rebuilt by the projector, then a pointer flip), not expand/contract.
- **Restore:** pause admissions (`control.pause`), PITR to `backup_ref`, deploy the prior image, resume. Local `read.db` keeps #7678's one-way policy unchanged.
- **Version skew:** api and controller N and N-1 coexist against an expand-compatible schema; a worker version is pinned per run, so stage images match their run's worker; plane contracts are additive.

<a id="cutover"></a>
### 12.5 One-time cutover (no history import, D-10) [APPROVED] (HA-P3-25)

0. **Readiness:** `goobers validate --target cloud` on the old instance and `goobers-cloud cutover check` on the new install report no blockers ([§8.4](#cutover-readiness)).
1. Stand up the new install (namespaces, Postgres, a fresh HA Temporal with the chosen shard count, object storage) with `goobers-cloud bootstrap`; admissions **paused**.
2. On the old instance: pause admissions; let open runs finish or cancel them (bounded by the longest run, or HITL holds resolved or abandoned by the operator); verify no claims are held, so nothing is double-claimed across instances.
3. Switch the webhook target and the trigger clients; unpause the new install.
4. Keep the old instance **read-only** (snapshot the RWO volume) for its retention window to serve history, then retire it.

**Rollback** until the old instance is retired: re-pause the new install, repoint the webhook and trigger clients, drain or cancel its runs and verify no claims are held, then unpause the old instance. Retiring the old instance is the point of no return.

An importer is only needed for history continuity, which is dropped as a constraint. If it is wanted later, a one-shot best-effort journal importer can be added as a non-gating tool.

---

<a id="temporal"></a>
## 13. Temporal tier

### 13.1 Workers [APPROVED] (HA-P2-03, HA-P2-04, HA-P5-08)

- **Stateless and DB-less.** The private work root moves to emptyDir; instance config comes **by digest** from the config plane (a live updater, keeping 3 superseded trees), not from the RWO PVC or a boot-time mirror seed; blobs come over the blob plane. Workers hold no DB credentials and no provider credentials.
- **Health.** `--health-addr`: readiness = pollers started **and** the current deployment version routes to this build (closes the #5407 wedge class); liveness = process only.
- **Concurrency.** `MaxConcurrentActivityExecutionSize` set explicitly and sized per pod.
- **Scaling.** On task-queue backlog / schedule-to-start latency, not CPU: the KEDA Temporal scaler if verified (UNVERIFIED), otherwise an external metric from `DescribeTaskQueue`. Minimum 2 workers per live version.

### 13.2 Versioning and rollout [APPROVED] (HA-P2-07)

- **Worker Deployments via the Temporal Worker Controller**, rainbow-style, one Deployment per build ID. `Run`, `DispatchOne` and `ChildDispatchOne` are **PINNED** (the registrations already declare it, `internal/engine/register.go:12-25`). An old version keeps its pods until its pinned runs drain, so **a rollout never interrupts an in-flight stage**. A plain rolling update is incompatible with versioning and is not used.
- **Fallback** if the Worker Controller is unsuitable: per-build Deployments managed by `goobers-cloud` release tooling, with current/ramping set via the versioning API.
- **Current-version automation (#4084):** a version with zero pollers is never made current (fixes the #5407/#5993 trap).
- **Retention cap:** 3 live versions, with AUTO_UPGRADE overrides for older pinned runs ([§10.5](#dsl-semantics)). The cost is counted: ≥6 idle worker pods at the cap, image retention for 3 builds, and one more operator.
- Activity, queue, update and query names never change in place; payloads stay additive.

### 13.3 Re-attach [APPROVED] (HA-P2-05)

Pinning protects rollouts, but node drains and evictions still kill worker pods. The dispatch activity's heartbeat carries `{namespace, pod, uid, attempt, custody}` (today only a sequence counter, `internal/engine/dispatchcancel.go:47-62`); on SIGTERM a worker returns `worker-handoff` **without deleting pods** (today it deletes them at `child_custody.go:49`); the retried activity adopts the live pod by uid ([§10.4](#reattach-semantics)). Once re-attach is green, worker grace can drop to 90s.

### 13.4 Upgrade ladder [APPROVED] (HA-P2-01)

From 1.27.2 / chart 0.62.0, first patch to 1.27.4, then advance one minor at a time through 1.28.4 / 0.67.0, 1.29.7 / 0.73.2 and 1.30.8 / 0.74.0, to **1.31.4 / chart 1.6.0** (Worker Deployment APIs became GA in 1.31.0; the ladder takes the latest patch in that minor). Each legacy step runs and awaits a schema-only Job before changing server pods; chart 1.6.0 supplies a pre-upgrade hook. Each step sets the versioning dynamic config, waits for every disposable History shard's range ID to advance and remain stable for 30 seconds, continues and replays a history started on the previous version, runs a fresh workflow, and records rollback. The ladder runs on the **current** single-replica Temporal, because P2 doesn't wait for P3.

### 13.5 Server HA [APPROVED] (HA-P3-22)

| Item | Spec |
|---|---|
| Services | frontend 3, history 3, matching 3, worker 2; web 2. Each its own Deployment with zone + host spread and PDB `maxUnavailable: 1` |
| Persistence | same PG server as Goobers, **separate databases** `temporal` and `temporal_visibility`, separate role `temporal_app` with no grants on `goobers_*` |
| Visibility | SQL advanced visibility on PG; non-authoritative for Goobers ("unknown ⇒ fail live", `internal/engine/liveness.go:73-75`) |
| `numHistoryShards` | **512**, fixed forever. About 1 history process per 500 shards; 512 fits an instance-dedicated cluster up to roughly low thousands of concurrently open workflows. 2048 only for one Temporal serving many instances. Validate with `omes` before cutover |
| Connections | direct 5432 (not the app PgBouncer); `maxConns` per service × replicas under `max_connections` with 30% headroom |

**Why the same server.** Goobers can't progress without Temporal, or vice versa, so separate servers don't raise the product's availability; they would double the HA cost and the backup and on-call story. Isolation comes from separate databases, roles and connection budgets. Split when measured I/O or connection contention says so.

### 13.6 Temporal Cloud

A supported option, not the reference. For: 99.9% contract (99.99% HA), no shard or upgrade burden; the payload codec (`internal/temporalcodec`) and TLS dial (`internal/temporaldial`) exist. Against: no Azure regions (cross-cloud latency UNVERIFIED, egress cost); Cloud HA RTO (<20 min) is worse than self-hosted zone-redundant HA; visibility List/Count has a 99.5% SLO and Goobers' liveness scans use it; sending execution metadata to a third party sits badly with the no-phone-home posture, so it is acceptable only as an explicit operator choice.

---

<a id="k8s-shape"></a>
## 14. Kubernetes shape

### 14.1 Shapes by phase [APPROVED]

| Workload | P0 (`goobers-system`, modes 2/3 today) | P3 (`goobers-cloud`) | P5 (per-role namespaces) |
|---|---|---|---|
| control plane | `strategy: Recreate`; pod grace = drain bound + 60s; liveness probe grace 15s; **single-replica PDB `maxUnavailable: 1`, non-blocking, not protective** | `control` Deployment ×1, `Recreate`, grace 30s, readiness = lease held + schema compatible; tolerations `node.kubernetes.io/{unreachable,not-ready}` `tolerationSeconds: 30`; preStop `goobers-cloud sleep 5`; PriorityClass `goobers-control-plane`; no PVC; **no harness, workspace or stage resources** (it never executes stages) | api ×3–8 (RollingUpdate 1/0, PDB maxUnavailable 1, AlwaysAllow); controller ×2 (RollingUpdate 1/0; readiness = can campaign) |
| worker | grace ≥ drain + child settlement + exit margin | rainbow per build (P2), PDB per version, readiness = pollers + routed, emptyDir work root | + KEDA |
| Temporal | 1 | HA fresh install | same |
| migrate | — | PreSync Job, `backoffLimit: 0` | in `goobers-ops` |

**Why P3 stays `Recreate`.** In the single `control` pod, readiness requires holding the lease. A RollingUpdate with `maxUnavailable: 0` would deadlock: the new pod can't become Ready until the old one releases, and the old one isn't terminated until the new one is Ready. `Recreate` plus voluntary release on SIGTERM plus a pre-pulled image gives the 30–60s target. The hot standby in P5 removes the gap.

<a id="namespaces"></a>
### 14.2 Namespace layout (P5) [APPROVED] (HA-P5-06)

| Namespace | Contents | Identity / DB role | NetworkPolicy (ingress ← / egress →) | Quota |
|---|---|---|---|---|
| `goobers-api` | api Deployment, Service, Ingress backend, HPA, PDB | SA `api` → Entra federated credential → `goobers_api` | ← ingress, stage namespaces, workers, controller; → PG 6432/5432, object storage, Temporal frontend, provider egress (credential mint), OTel | pods ≤12 |
| `goobers-controller` | controller Deployment (2), PDB | SA `controller` → `goobers_controller` | ← observability; → PG, Temporal frontend, provider egress, object storage | small fixed |
| `goobers-workers` | Worker Controller, versioned worker Deployments, PDBs, scaler | SA `worker`: **no DB role**; k8s Role in stage namespaces only | ← observability; → Temporal frontend, api (planes, config, blobs), k8s API | versions × replicas |
| `goobers-stages-<gaggle>` | stage pods (bare, one per attempt) | no SA token; run-scoped bearer from the plane | default-deny; → api Service only, + egress allowlist via `goobers-egress` | **per gaggle** (CPU, memory, pod count, `goobers-stage` priority only) |
| `goobers-temporal` | Temporal services, web | SA → `temporal_app` | ← workers, api, controller, observability; → PG | fixed |
| `goobers-data` | CNPG Cluster + operator PDBs (absent on managed PG) | CNPG-managed | ← api, controller, ops, temporal; → object storage (Barman) | fixed |
| `goobers-ops` | migrate Job, maintenance CronJobs | SA `migrate` → `goobers_owner` (DDL) | → PG, backup API / CNPG `Backup` CR | Jobs only |
| `goobers-observability` | OTel collector, metrics scraping | read-only SAs | ← all (OTLP); → user-configured exporters | fixed |
| `goobers-egress` | egress proxy (audit/FQDN layer) | none | ← stage namespaces; → allowlisted internet | fixed |

**PriorityClasses:** `goobers-control-plane` (100000) for api, controller, workers, Temporal and migrate; `goobers-stage` (1000, `preemptionPolicy: Never`) for stage pods. Stage namespaces carry a ResourceQuota `scopeSelector` that forbids `goobers-control-plane`.

**Blast radius:** a compromised stage pod reaches only the api Service, with a bearer scoped to its run and stage; a compromised worker can create pods only in stage namespaces and holds no DB or provider credentials; a compromised api pod has DML but no DDL and no k8s token, and column grants keep leader-only writes from it; a compromised Temporal pod can't reach `goobers_*` databases. Quotas stop one gaggle from starving the control plane.

### 14.3 Per-workload spec (P5) [APPROVED]

| Workload | Replicas | Strategy | PDB | Readiness | Liveness | Grace / preStop | Spread |
|---|---|---|---|---|---|---|---|
| api | 3–8 (HPA) | RollingUpdate maxSurge 1, maxUnavailable 0; `minReadySeconds: 10` | maxUnavailable 1, AlwaysAllow | plane-ready **and** schema-compatible. Not a per-probe DB ping: handlers answer 503 `recovering` during a DB outage | in-memory heartbeat; probe grace 15 | 60s; preStop `sleep 10`; on SIGTERM flip `/readyz` to 503, finish in-flight, hand off owned runs, close SSE (clients resume via `Last-Event-ID`) | zone `ScheduleAnyway`, host `DoNotSchedule` |
| controller | 2 | RollingUpdate 1/0 | maxUnavailable 1, AlwaysAllow | process up and able to campaign (the standby **is** Ready) | leader: loop heartbeat fresh; standby: always OK; probe grace 15 | 30s; SIGTERM ⇒ stop loops, release lease, exit | zone |
| worker vN | N (min 2) | Worker Controller rainbow | maxUnavailable 1 per version, AlwaysAllow | pollers up **and** routed | process; probe grace 30 | 90s once re-attach is green | zone |
| Temporal svc | 3/3/3/2 | RollingUpdate 1/0 | maxUnavailable 1 each | chart | chart | chart | zone + host |
| migrate | Job | — | — | — | — | `backoffLimit: 0`, `activeDeadlineSeconds: 1800` | — |
| stage pod | 1 per attempt | — | none (bare pod; `activeDeadlineSeconds` backstop) | — | — | 120s isolated children [EXISTING] | — |

`unhealthyPodEvictionPolicy` is GA in k8s 1.31 and the preStop `sleep` action is GA in 1.34; before 1.34 a distroless image can't exec `sleep`, so `goobers-cloud sleep N` ships as a subcommand. **Never** use `minAvailable: 1` on a single replica: it blocks AKS drains.

**Stage pods vs node drains.** AKS force-drains after its timeout (30 min default). With re-attach the retried activity adopts a surviving pod; if the pod died with the node, it retries as an infra attempt. Set the node-pool drain timeout ≥ the longest typical stage, and document it.

### 14.4 Probe rules

1. **Liveness never checks dependencies** (DB, Temporal); otherwise a DB blip restarts every pod.
2. **Readiness is about this pod, not the leader.** The controller standby is Ready.
3. **Liveness kills must not inherit the pod grace (KEP-2238).** Set probe-level `terminationGracePeriodSeconds` everywhere.

### 14.5 Drain and rollout semantics

- **api:** SIGTERM → `/readyz` 503 → preStop 10s (EndpointSlice/LB convergence) → registry handoff for owned runs → finish in-flight requests ≤40s → close. Mid-flight stage-pod requests are idempotent and replay on a peer.
- **controller (leader):** SIGTERM → stop L1/L2/L3 → release the lease (epoch+1) → exit (≤5s). The standby acquires and runs **rebuild-before-reap** (renewal set from `control.claim` × Temporal open workflows, *then* `RecoverExpired`), then reconciles uncertain acceptances by `owner_epoch`.
- **worker:** rainbow, so old pods drain naturally; on an eviction, handoff + re-attach.

### 14.6 YAML sketch (abridged)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: goobers-api}
spec:
  replicas: 3
  strategy: {type: RollingUpdate, rollingUpdate: {maxSurge: 1, maxUnavailable: 0}}
  minReadySeconds: 10
  template:
    metadata: {labels: {app.kubernetes.io/name: goobers-api}}
    spec:
      serviceAccountName: api                 # federated to Entra → PG role goobers_api
      automountServiceAccountToken: false
      priorityClassName: goobers-control-plane
      terminationGracePeriodSeconds: 60
      topologySpreadConstraints:
        - {maxSkew: 1, topologyKey: topology.kubernetes.io/zone, whenUnsatisfiable: ScheduleAnyway,
           labelSelector: {matchLabels: {app.kubernetes.io/name: goobers-api}}, matchLabelKeys: [pod-template-hash]}
        - {maxSkew: 1, topologyKey: kubernetes.io/hostname, whenUnsatisfiable: DoNotSchedule,
           labelSelector: {matchLabels: {app.kubernetes.io/name: goobers-api}}, matchLabelKeys: [pod-template-hash]}
      containers:
        - name: api
          image: goobers-cloud@sha256:…
          args: [api]
          lifecycle: {preStop: {sleep: {seconds: 10}}}
          readinessProbe: {httpGet: {path: /readyz, port: https, scheme: HTTPS}, periodSeconds: 5, failureThreshold: 2}
          livenessProbe:  {httpGet: {path: /healthz, port: https, scheme: HTTPS}, periodSeconds: 15, failureThreshold: 4,
                           terminationGracePeriodSeconds: 15}
          resources: {requests: {cpu: 250m, memory: 512Mi}, limits: {memory: 1Gi}}
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: {name: api, namespace: goobers-api}
spec: {maxUnavailable: 1, unhealthyPodEvictionPolicy: AlwaysAllow,
       selector: {matchLabels: {app.kubernetes.io/name: goobers-api}}}
---
apiVersion: v1                                # stages may never use control-plane priority
kind: ResourceQuota
metadata: {name: no-control-plane-priority, namespace: goobers-stages-example}
spec:
  hard: {pods: "0"}
  scopeSelector: {matchExpressions: [{operator: In, scopeName: PriorityClass, values: [goobers-control-plane]}]}
---
apiVersion: batch/v1                          # Argo PreSync / Helm pre-upgrade
kind: Job
metadata:
  name: goobers-migrate-<release>
  namespace: goobers-ops
  annotations: {argocd.argoproj.io/hook: PreSync, argocd.argoproj.io/hook-delete-policy: BeforeHookCreation}
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 1800
  template:
    spec:
      serviceAccountName: migrate
      restartPolicy: Never
      containers:
        - name: migrate
          image: goobers-cloud@sha256:…        # the NEW release
          args: [migrate, apply, --require-backup, --mode=expand]
```

No k8s Lease or Lease RBAC anywhere; the controller has `automountServiceAccountToken: false`; the worker has no instance PVC.

---

<a id="security"></a>
## 15. Security

- **DB auth.** Azure: Entra workload identity per SA → a distinct PG role per role (`goobers_api`, `goobers_controller`, `goobers_owner`, `temporal_app`). pgx fetches an Entra token in `BeforeConnect` (scope `https://ossrdbms-aad.database.windows.net/.default`). App traffic uses the built-in PgBouncer (6432, transaction mode) with `QueryExecModeExec` and no named prepared statements; the notifier and migrate use 5432. Token auth via 6432 is tested explicitly (reported unreliable elsewhere; UNVERIFIED with pgx). Private networking; `publicNetworkAccess` disabled (VNet-integrated in `postgres.bicep` since HA-P3-21; it was `Enabled` at the baseline). CNPG: operator-managed per-role Secrets or client certificates; never a superuser for apps.
- **Grants.** Generated from the write-path inventory ([§11.2](#lease)). `goobers_api` has no DDL and no write on leader-only columns; `goobers_controller` has no DDL; `goobers_owner` is used only by the migrate Job.
- **Secrets.** Key Vault CSI (or k8s Secrets) holds versioned pod-token keys, the webhook HMAC secret and provider app keys, mounted into **api and controller only**. Workers and stage pods get none. There is **no scrub key**: the database and its backups hold no secret material, raw, encrypted or fingerprinted ([§11.4](#scrubber)).
- **No stages in the control plane (D-2).** `selfExecution: deny` is enforced at boot; the `control` / api / controller pods mount no harness, workspace or stage credentials; `internal/runner` is not linked into `goobers-cloud`.
- **Plane bearer keys** (HA-P3-17). Signed tokens carry a `kid`; verifiers hold {current, previous} and mint with current; the key file is watched and reloaded. Rotation: add new as previous → roll → promote → roll → drop old only after at least the longest grant TTL (currently **24h**), counted from when every replica has stopped minting with the old key. Pod tokens alone expire within 8h; credential-refresh and child-workflow grants determine the longer overlap. Registry mode (`podauth.go:144-155`) is refused at boot in cloud. The credential-refresh grant key is derived from the shared key in its own MAC domain (HA-P0-06); revocations go to `control.grant_revocation`.
- **Internal proxy channel** (P5 run-owner routing and registry handoff): a service bearer in a new `podauth` MAC domain; NetworkPolicy allows it only between pods in `goobers-api`; handoff payloads are never logged or persisted.
- **Webhook secret:** dual-secret verify during rotation; dedup by source receipt makes redeliveries harmless.
- **Network:** default-deny in every namespace; stage pods reach the api Service only; workers have no DB route.
- **Shared-DB blast radius.** One server hosts `goobers_<instance>`, `temporal` and `temporal_visibility`, with separate roles and no cross-database grants. A compromised app role reads its own instance's journal (already scrubbed) and control state; it can't alter schema or leadership, read Temporal payloads (encoded by `temporalcodec`), or touch other instances' databases.

---

<a id="failure-modes"></a>
## 16. Failure modes

**Expected** = behaviour under this design. Each line is a gating test; "first gated" names the phase.

| # | Scenario | Expected behaviour | Gate | First gated |
|---|---|---|---|---|
| F1 | Leader node loss | P3: the single pod is rescheduled (~1.5–2.5 min). P5: the lease expires on the DB clock (≤15s); the standby acquires epoch+1, runs rebuild-before-reap and resumes admission in ≤60s. No live claim reaped; stage pods ride the gap; seq contiguous; one `run.started` per acceptance | CHAOS-1 (+ node-kill variant) | P3 (reported), P5 |
| F2 | Rolling update / eviction | P3: `Recreate` pause 30–60s, 0 infra attempts. P5: api readiness drop + preStop, peers absorb, SSE resumes by cursor; controller handover ≤5s | RESTART-RTO, CHAOS-2 | P3, P5 |
| F3 | Partitioned zombie leader | Renew fails ⇒ self-fence at 10s; any slipped write fails the epoch check; a duplicate start returns **NotMine** with no claim, slot or journal write; renew unaffected by fenced transactions; rejoins as standby | CHAOS-3 | P3 (unit + kind), P5 |
| F4 | DB failover | In-flight transactions ambiguous; handlers return 503 `recovering`; leases lapse and exactly one controller acquires after failover; every 2xx write survives (RPO 0); retries dedup; the time-based client budget outlasts the 60–120s window | CHAOS-4 | P3 |
| F5 | Temporal frontend outage (10 and 35 min, handover mid-way) | Admission pauses; renewal treats "unknown" as live, so 0 reaps; HITL 503 → client retries with the same `UpdateID`; reads stay up; no duplicate start after recovery | CHAOS-5 | P6 |
| F6 | Stage pod loses the API mid-write | Same key to any replica ⇒ exactly once; bare 502/504 ambiguous and retried only when replay-safe; exhaustion ⇒ `infra`, never `policy`; artifact records use the plane budget | CHAOS-6 | P0 (client), P3 |
| F7 | Clock skew ±10 min on a replica | All leases and expiries use DB `now()`; token `exp` tolerance bounded (±2 min), beyond which 401 | CHAOS-7 | P5 |
| F8 | Stuck leader (SIGSTOP 1.5 × TTL, **by PID from the pod cgroup or `kubectl debug`, never by process name**) | The standby takes over during the pause; after SIGCONT the first leader write is fenced and the old leader demotes within one renew interval | CHAOS-8 | P5 |
| F9 | Webhook redelivered concurrently to two replicas | One source receipt, one run per recipient, both 2xx with the same acceptance; a tampered body under the same delivery ID ⇒ 409 | CHAOS-9 | P3 |
| F10 | Cron at handover (20× at :59.9 with jitter) | One acceptance per cursor advance (source receipt + CAS); catch-up per policy | CHAOS-10 | P3 (restart), P5 (two schedulers) |
| F11 | HITL wait spanning an upgrade | The pinned run stays on worker vN; a resolution sent twice to two replicas is applied once; no non-determinism error | UPGRADE-11 | P2 |
| F12 | Migration mid-rollout | N-1 pods keep serving on the expand schema; one migration row; SSE cursors valid or a single 409 | UPGRADE-12 | P6 |
| F13 | Worker eviction mid side-effecting stage | Handoff → retry adopts the live pod by uid → stage completes; no second pod; attempt not charged; deadline preserved | CHAOS-13 | P2 |
| F14 | AKS node-pool upgrade under load (33% surge, all pools) | No PDB blocks the upgrade; all targets hold; zero policy-class attempts | CHAOS-14 | P5 (release tier), P6 |
| F15 | Zone loss | api/Temporal survive on 2 zones; DB promotes the zone-redundant standby; controller failover per F1; RTO = max(DB, controller) ≤3 min | CHAOS-15 | P6 |
| F16 | Run owner crash (P5) | Successor acquires epoch+1; registry gap bounded by credential TTL; pattern scrubbers backstop; no seq gap or duplicate | nightly kind | P5 |
| F17 | Run owner graceful rollout (P5) | Registry handed off in memory; zero unredacted registered values in the corpus | `TestScrubRegistryHandoffOnSigterm` | P5 |
| F18 | Local↔cloud divergence | Equivalence corpus diff empty | `TestCloudJournalEquivalesLocal` | P3 |
| F19 | Ineligible workflow reaches cloud (2.0, unsupported feature, or self placement) | Config apply refuses the generation with `RUNTIME00x`; the running generation stays applied; nothing is admitted; no partial run | `TestCloudConfigApplyRefusesIneligibleWorkflow`, `TestEngineOnlyAdmissionGuard` | P3 |

**Cross-cutting assertions** (evidence bundle per run; an invalid capture is not a pass):
- no `(run, emit_key)` duplicates; `(run, branch, seq)` contiguous;
- no claim with two run IDs over overlapping intervals;
- one acceptance per source key and ordinal;
- no overlapping lease intervals across holders;
- no `NotMine` start produced a claim, slot or journal write;
- no emit committed with a stale `owner_epoch`;
- no stage executed by a control-plane pod;
- no `policy`-class attempt caused by an injected infra fault;
- an empty A2 conformance diff vs a fault-free run.

---

<a id="phases"></a>
## 17. Phases: entry criteria, scope, exit gates

**Rules for every phase:**
- **Main stays releasable.** `goobers-cloud` is built in CI but **excluded from release artifacts** until P3 exits. Nothing in `goobers` is feature-flagged. Each refusal removal ships in the same PR as its qualification.
- **Exit gates are named tests**, green on main for 7 consecutive nightly runs where the gate lives in the nightly tier.
- **High-churn shared refactors land alone** (`localscheduler`, `triggerqueue`, `journal`, `engine`). No parallel auto-merges touch the same package.
- **Per-phase details are confirmed at phase start**, together with that phase's D-4…D-10 defaults.

<a id="p0-fix-now"></a>
### P0: Fix now (old topology, no HA)

- **Entry:** none.
- **Scope and status (2026-10-21):**

| Issue | Work | Status |
|---|---|---|
| HA-P0-01 | CI: cache the Temporal CLI, 429-aware backoff | PR #7688 (open) |
| HA-P0-02 | api base manifest: Recreate, bounded grace, probe-level liveness grace, non-blocking PDB | PR #7689 (open) |
| HA-P0-03 | worker grace covers drain + child settlement in the preparer; README text | PR #7689 (open) |
| HA-P0-04 | merge-lock: one exclusion domain for file and plane backends | PR #7691 (open) |
| HA-P0-05 | terminal-report outbox: single replay owner across processes | PR #7693 (open) |
| HA-P0-06 | credential-refresh grant key derived from the shared pod-token key | PR #7692 (open) |
| HA-P0-07 | planehttp: time-based budget, ambiguous bare 5xx, infra on exhaustion, request keys | PR #7690 (in review) |
| HA-P0-08 | daemon drain bounded at a stage boundary | not started |
| HA-P0-09 | doc drift: DS §10 Temporal Schedules text; `RunScheduled` inverse is legacy | done in PR #7689 |

- **Exit gate:**
  - `TestPlaneRetryBudgetIsTimeBased`, `TestClassifyBareProxy5xxAmbiguous`, `TestPlaneExhaustionIsInfraEvenAtStageDeadline`, `TestNXDOMAINFailsFast`, `TestReleaseWithReportReplaySafeWithKey`;
  - `TestDeployReferenceAPISingletonShutdownAndDisruption`, `TestPreparedWorkerGraceCoversDrainAndSettlement`;
  - `TestClaimLedgerFileAndPlaneMergeLocksShareExclusion`, `TestReplayLockExcludesProcessesAndReleasesAfterCrash`, `TestCredentialGrantKeyIsSharedAndDomainSeparated`, `TestDaemonDrainStopsAtStageBoundary`;
  - the existing restart suite green (`TestUpDoesNotReapEngineLiveClaimAcrossRestart`, `TestRecoveryGateBlocksUntilRenewalRebuilt`, `TestRenewLiveClaimsLedgerDrivenAcrossRestart`);
  - `TestTemporalCLIIsAcquiredOnceAndRestoredByTestJobs`, and CI logs show Temporal CLI cache hits in unit shards and the integration job.
- **Gain:** a restart no longer burns attempts; the base no longer deadlocks; worker grace covers the 30s drain + 5-min child settlement. HA-P0-08 then replaces the interim 60-minute drain bound (PR #7689) with a stage-boundary drain, so a deploy's admission pause stops depending on the longest in-flight run.

<a id="p1-extraction"></a>
### P1: Extraction and seams (no behaviour change)

- **Entry:** HA-P0-01 merged; the dependency record (HA-P1-01) approved before any PR adds pgx or azblob; the `cloud-pg` job (HA-P1-02) and closure rule (HA-P1-04) live before the first `internal/cloud` package; D-4 and D-7 confirmed.
- **Scope:** HA-P1-01 … HA-P1-14. Order: HA-P1-09 (alone, first) → HA-P1-06/07/08 (each alone) → HA-P1-10/11 (alone) → HA-P1-12 → HA-P1-13 → HA-P1-14.
- **Exit gate:** `make ci` green with **no golden, digest or parity changes**; `TestJournalBackendConformance/file`, `TestSchedulerPolicyStoreSuite/file`, `readmodel/conformance`; `TestGoobersClosureExcludesCloudDeps`; `cmdgoobersgrowth` measures `cmd/goobers-cloud`; deadcode clean (stores wired into the `goobers-cloud` skeleton, no dated exemptions); `TestLocalJournalEquivalenceBaseline` records the corpus.
- **Gain:** none user-visible; `cmd/goobers` shrinks by about 10–15k lines.

<a id="p2-reattach"></a>
### P2: Worker re-attach and rainbow rollouts

- **Entry:** P0 merged; HA-P2-02 (nightly kind) live first; the Temporal ladder target picked (HA-P2-01). P2 does **not** wait for P1 or P3: it runs on today's topology with workers moved off the RWO volume (HA-P2-04).
- **Scope:** HA-P2-01 … HA-P2-08.
- **Exit gate:** `TestDispatchReattachAdoptsLivePodByUID`, `TestReattachPreservesAttemptDeadline`, `TestHandoffDoesNotChargeAttempt`; the heartbeat-contract-v3 replay test over a captured pre-change history; `TestWorkerReadinessFailsWhenUnrouted`; **CHAOS-13** and **UPGRADE-11** green in nightly kind for 7 nights; `UPGRADE-HITL` replay test.
- **Gain:** 0 stage restarts from worker rollouts; 0 side-effecting failures from worker drains.

<a id="p3-state"></a>
### P3: State off RWO, single controller, engine-only admission, cutover

- **Entry:** P1 exit; P2 at least HA-P2-05 merged (re-attach is required for rainbow workers in the new install); D-6, D-8 and D-10 confirmed. The DSL 3.0 lock is **not** required to build P3 ([§8.2](#engine-only-admission)).
- **Scope:** HA-P3-01 … HA-P3-27.
- **Exit gate:**
  - **cloud-pg (required, path-filtered):** PG store suites; `TestSchedulerPolicyStoreSuite/pg`; `TestJournalBackendConformance/pg`; `readmodel/conformance` on PG; `TestLeaseRenewNotBlockedByFencedTx`; `TestFencedWriteRejectsStaleEpoch`; `TestColumnGrantsMatchWritePathInventory`; `TestZombieStartIsNotMine`; `TestReconcileAdoptsPredecessorStart`; `TestAcceptanceKeyReuseAfterRetentionStartsNewRun`; `TestJournalSeqContiguousUnderConcurrentEmit`; `TestNoNotifyInEmitTransaction`; `TestOldBinaryServesExpandSchema`, `TestMinCompatibleBinaryIsFirstNonReader`; `TestCloudConfigApplyRefusesIneligibleWorkflow`; `TestGoobersCloudClosureExcludesRunner`; **local↔cloud journal equivalence**.
  - **Nightly kind:** CHAOS-4 (CNPG primary failover), CHAOS-6, CHAOS-9, CHAOS-10 (restart variant), `RESTART-RTO` (admission resumes ≤60s after a rollout, p95 of 20 rollouts, **reported**), a node-kill variant (≤2.5 min, reported).
  - **Manual:** D11 smoke on `goobers-cloud`; cutover dry-run on kind; **one production instance that passes the readiness check** cut over and stable for 14 days.
- **Cutover dependency (D-2).** The 14-day production cutover needs an instance whose workflows are all cloud-eligible. For an instance on the reference gaggle that means DSL ≥3.0 workflows (#7616 or the operator's own `goobers fix --to 3.0`) and HA-P4-04…06 (parallels, selection). Removing a lane from an instance's config to make it eligible is the operator's choice, not a design feature.
- **Gain:** deploy pause 30–60s; node loss ~1.5–2.5 min with no disk detach; DB failover ridden out; HA Temporal.

<a id="p4-parity"></a>
### P4: Engine parity and cutover readiness (parallel with P2–P3)

- **Entry:** HA-P4-01, HA-P4-02, HA-P4-03: none (D-1 approved). HA-P4-04…15: HA-P4-03 merged; each item starts with a readiness-check run on the instances planned for cutover, confirming the order. HA-P4-16: DSL 3.0 locked (#7590) and #7616 merged.
- **Scope:** HA-P4-01 … HA-P4-16 ([§8.5](#engine-parity)).
- **Exit gate:** per feature, a parity row, a replay test and recovery tests; `TestRuntimeSupportColumnMatchesEngineRefusals` with an **empty** engine-unsupported set; every shipped reference workflow (after #7616) green on the engine in nightly kind; CHAOS-13 extended to parallels and child workflows.
- **Gain:** each item unblocks the cutover of instances that use its feature; at exit, one DSL runs everywhere.

<a id="p5-ha"></a>
### P5: Controller HA, N api replicas, per-role namespaces

- **Entry:** P3 exit with 14 days stable in production; P2 exit; D-5 and D-9 confirmed; an enforcing CNI available in nightly kind (HA-P5-07).
- **Scope:** HA-P5-01 … HA-P5-10.
- **Exit gate:**
  - **Nightly kind:** CHAOS-1, CHAOS-2 (api and controller), CHAOS-3, CHAOS-7, CHAOS-8, CHAOS-10 (two live schedulers); the NetworkPolicy reachability matrix on Calico; F16, F17.
  - **cloud-pg:** `TestRunOwnerAffinityRedactsMintedCredential`, `TestScrubRegistryHandoffOnSigterm`, `TestStaleOwnerEmitRejected`.
  - **Release tier:** CHAOS-14 on AKS (dormant until provisioned; must pass before P5 is declared done in production).
- **Gain:** handover ≤5s, failover ≤60s (Temporal reachable), zero-downtime api, webhook, HITL and SSE ingestion.

<a id="p6-certification"></a>
### P6: Mode-3 removal and certification

- **Entry:** P4 exit (all 8 features), DSL 3.0 locked, #7616 merged; P5 exit; the mode-3 deprecation window elapsed (one released minor). HA-P6-02 (outbox) only if the duplicate hook-effect rate measured in HA-P5-04 is above threshold, **or** a planned change needs hooks inside the workflow.
- **Scope:** HA-P6-01 … HA-P6-08.
- **Exit gate:** UPGRADE-12, CHAOS-5, CHAOS-14, CHAOS-15, a recorded `omes` sizing run, a 24h soak with all cross-cutting assertions green (self-hosted runner), the PITR restore drill, the GC PITR-safety property test.
- **Gain:** one mode-3 implementation; a published 99.9% SLO.

---

<a id="ci-plan"></a>
## 18. CI plan

### 18.1 Today, measured [EXISTING] (sample of 8 successful PR runs, 2026-10-20)

- The only required check is the `required-ci` aggregate. There is no merge queue; pushes to main re-run the matrix.
- 5 unit race shards (LPT over `.github/unit-shard-weights.json`, test-level splits in `.github/unit-shard-splits.json`).
- PR wall clock 26–37 min, about 204 job-min per PR. The long pole is the Windows gate (~27 min); unit shards take 9–18 min.
- Queue wait was ~0 in the sample. A long queue, if it happens, is a peak-period effect that must be re-measured with job-level timings before anyone designs around it. The measured cost driver is test runtime and the long pole.
- Not present: Postgres, service containers, kind/k3d. The Temporal CLI was downloaded fresh per shard (`internal/temporaltest/devserver.go`: `CachedDownload{Version: "v1.8.2"}`, no Actions cache, no 429 backoff); HA-P0-01 fixes this.

### 18.2 Tiers [APPROVED]

| Tier | Contents | Where | Est. cost | Gates |
|---|---|---|---|---|
| **Required, every PR** | pure unit/property tests: lease/fence logic against a fake clock, start-fence decision table, retry classifier, acceptance state machine, migration tag guard, manifest semantic render tests, runtime-column equality, readiness-check goldens, closure rules, local equivalence baseline, parity rows | existing 5 shards | **+3–6 job-min**; wall clock unchanged. Refresh shard weights after P1 and P3 | P0, P1, P4 |
| **Required, path-filtered `cloud-pg`** | `services: postgres:16`, Azurite, **cached** Temporal CLI. Store suites, PG legs of the shared suites, journal/readmodel conformance on PG, N-1/N migration, role e2e, **local↔cloud equivalence**. Path filter: `cmd/goobers-cloud/**`, `internal/cloud/**`, `migrations/**`, and the shared seams `internal/{localscheduler,triggerqueue,journal,livejournal,readmodel,engine,engineadmission,startintent}/**` | ubuntu hosted, parallel to the shards | **+10–15 job-min on matching PRs**, ≤15 min wall (under the Windows pole) | P3, P5 |
| **Nightly** | kind + Calico (enforcing) + CNPG ×3 + Temporal (HA values) + `goobers-cloud`; CHAOS-1…10, 13, UPGRADE-11/12, F16–F19; netpol matrix; PG 17 leg; restart-RTO, emit-throughput and scrub benchmarks **reported, never asserted** | hosted 4-vCPU, 90-min timeout | **45–90 min/night**, wired to `scheduled-failure-alarm` + `flake-watch` **with a named owner rotation** | P2, P3, P5 |
| **Release / manual** (dormant until provisioned) | AKS + Azure PG GP-ZR + Entra via PgBouncer 6432 + live Blob; CHAOS-14, CHAOS-15; `omes`; 24h soak | AKS test environment via OIDC (never on fork PRs); **self-hosted runner** for the soak (hosted jobs cap at 6h) | cluster cost; not on the merge path | P5 (CHAOS-14), P6 |

**Merge impact:** required wall clock unchanged; job-min rises about 5–20% on matching PRs, and main pushes double that (no merge queue).

### 18.3 CI issues (9)

| Issue | Phase | What |
|---|---|---|
| HA-P0-01 | P0 | Actions cache for the Temporal CLI, `GOOBERS_TEMPORAL_CLI` in unit, integration and (later) `cloud-pg` jobs, 429-aware backoff; runtime-acquisitions inventory. **First** |
| HA-P1-02 | P1 | `cloud` scope profile + `cloud-pg` job; joins `required-ci` as path-conditional |
| HA-P1-03 | P1 | `cmdgoobersgrowth` generalized to every `cmd/*` main |
| HA-P1-04 | P1 | `architectureimports` closure rules ([§9.3](#local-impact)) |
| HA-P1-05 | P1 | gate strategy: deadcode via the `goobers-cloud` skeleton, complexity re-baseline with each move, coverage headroom plan |
| HA-P2-02 | P2 | nightly kind environment: Calico, CNPG, image build+load, toxiproxy as a CI-acquired binary, Temporal values |
| HA-P3-24 | P3 | local↔cloud equivalence leg, `cloud-pg` timing budget, shard-weight refresh |
| HA-P5-07 | P5 | NetworkPolicy reachability matrix on the enforcing CNI |
| HA-P6-06 | P6 | release-tier AKS environment, OIDC federation, self-hosted soak runner |

### 18.4 Which tests gate which phase

| Test / suite | Tier | P0 | P1 | P2 | P3 | P4 | P5 | P6 |
|---|---|---|---|---|---|---|---|---|
| planehttp table, CHAOS-6 client cases | required | ● | | | ● | | | |
| existing restart suite | required | ● | ● | | | | | |
| journal goldens / parity rows / `make ci` unchanged | required | | ● | ● | ● | ● | ● | ● |
| store suites (file) | required | | ● | | | | | |
| store suites (pg), conformance on PG, migrations | cloud-pg | | | | ● | | ● | |
| local↔cloud equivalence | required + cloud-pg | | ● (local) | | ● | ● | ● | ● |
| runtime column, readiness check, engine-only admission | required + cloud-pg | | | | ● | ● | | |
| re-attach unit + replay | required | | | ● | | | | |
| CHAOS-13, UPGRADE-11 | nightly | | | ● | | ● | | |
| CHAOS-4/6/9/10 (restart), RESTART-RTO | nightly | | | | ● | | | |
| per-feature parity + recovery | required + nightly | | | | | ● | | |
| CHAOS-1/2/3/7/8/10, netpol matrix, F16–F17 | nightly | | | | | | ● | |
| CHAOS-14 | release | | | | | | ● | ● |
| UPGRADE-12, CHAOS-5/15, omes, 24h soak, PITR drill | nightly / release | | | | | | | ● |

---

<a id="work-breakdown"></a>
## 19. Work breakdown and honest size

Issues are keyed `HA-Pn-NN` and filed under one epic with one tracking issue per phase. The epic, the phase trackers and every P0 and P1 issue are filed now; the rest are filed when their phase starts, with details confirmed then.

Estimates are non-test lines (±40%), from baselines at `f42351fb1`: `readmodel` 13.5k lines; `triggerqueue` 4.0k plus child tables; `journal` 12.8k; the plane files in `cmd/goobers` about 15k (`up.go` 2,678, `dispatchexec.go` 1,328, `providercmd.go` 1,150, `runnerwiring.go` 991, `writeplanes.go` 849, `enginerunguards.go` 714, `engineterminalhooks.go` 534, …); `cmd/goobers` has 591 non-test files. The engine's test:code ratio is about 2.7:1.

| Phase | Issues | of which CI | of which change the `goobers` binary | Non-test LOC | Test LOC | PRs (est.) |
|---|---|---|---|---|---|---|
| P0 | 9 | 1 | 5 | 1.5–2.5k | 2–4k | 10–12 |
| P1 | 14 | 4 | 7 | 5–9k new + 10–15k moved | 7–12k | 24–32 |
| P2 | 8 | 1 | 4 | 4–7k + 1–2k YAML | 6–12k | 14–20 |
| P3 | 27 | 1 | 9 | 27–44k (incl. 4–6k YAML/Bicep) | 40–75k | 50–70 |
| P4 | 16 | 0 | 15 | 15–25k | 31–57k | 32–42 |
| P5 | 10 | 1 | 3 | 7–13k (incl. 3–5k YAML) | 11–22k | 14–20 |
| P6 | 8 | 1 | 2 | 5–9k new, 6–12k deleted | 9–18k | 11–16 |
| **Total** | **92** | **9** | **45** | **~66–112k new** | **~105–200k** | **~155–210** |

Plus 3 issues that change local test fixtures only (HA-P0-02, HA-P0-03, HA-P1-14), counted in the totals but not in the binary column.

**Duplicated concepts, each under a shared suite:**

| Concept | Implementations | Shared suite |
|---|---|---|
| claims, scheduler state, acceptance/source receipts/cursor/demand | file/SQLite + PG | `TestSchedulerPolicyStoreSuite` (file, pg) |
| run journal + instance log | file + PG | `TestJournalBackendConformance` (file, pg) + local↔cloud equivalence |
| read model | SQLite + PG | `readmodel/conformance` |
| config generations, recovery records | files + object store/PG | `configgeneration` and `recovery` conformance (HA-P3-13/14) |
| terminal-report outbox | files + PG | `terminalreport` conformance (HA-P3-14) |
| runner walk vs engine walk | `internal/runner` + `internal/engine` | parity rows + equivalence corpus (P4 extends both) |
| stall sweep, run IDs, scrubber, terminal hooks | **one implementation**, shared code | existing suites |

---

<a id="departures"></a>
## 20. Departures from approved designs

| # | Approved text | Departure | Status |
|---|---|---|---|
| 1 | Goobernetes D5 / goobernetes-architecture §4: "The daemon is the control plane… one writer service owns sequence allocation"; DS1: "One daemon… no second replica in v1"; k8s-infra-shape §4: "No HA" | Cloud control plane = `api` ×N + `controller` ×2 (one active, fenced) from P5. D5's *ownership* (claims, quota, caps and fairness are Goobers-owned, not Temporal) is preserved | Approved in direction |
| 2 | DS §10 / DS7: "Failover is deferred"; pod loss is an accepted outage | Lifted: failover ≤60s, handover ≤5s (P5) | Approved in direction |
| 3 | DS4 / D5: the daemon's single writer owns sequence allocation | The **database** owns it: per-run counter row + unique `emit_key` | Approved in direction |
| 4 | #7678: one-way migrations + refuse-newer | Local unchanged. Cloud: expand/contract + `min_compatible_binary`, migrate Job, backup or PITR mark first; no down-migrations | Approved in direction |
| 5 | Run-ID minting random on direct paths (`scheduler.go:228-229`) | **No semantic departure:** cloud makes the existing acceptance-row ID mandatory for every start path | Approved in direction |
| 6 | Goobernetes D1 / goobernetes-architecture §1–2: "Three modes, one DSL, one binary"; "a workflow must not care where it runs" | **Two binaries** (D-4). "One DSL, one repo" kept and enforced by P4, the runtime column and the equivalence test. Until each P4 item ships, cloud **refuses** workflows that use that feature instead of running them (D-2), and the readiness check makes that visible before cutover | D-2 approved; D-4 default adopted |
| 7 | Mode 3 permits `self` lanes and runner-path HITL | Cloud never executes stages: `selfExecution: deny` mandatory from the first cloud release; HITL and cancel go to Temporal | Approved (D-2) |
| 8 | Engine terminal hooks run in the daemon (`awaitInto`) | Leader-owned `awaitInto` with fenced hooks (P5); terminal intent + outbox only if HA-P6-02's criterion holds | Approved in direction |
| 9 | DI-6 tag equality; O-1 skew window "deliberately not designed in v1" | N/N-1 for api/controller; per-run pinned worker version for stage images | Approved in direction |
| 10 | k8s-infra-shape §2: one `goobers-system` namespace | Per-role namespaces (P5); `goobers-system` stays the mode-2 base | Approved in direction |
| 11 | DS §9 M5 guard as second-daemon protection | Retired in cloud (replaced by the fence); kept locally | Approved in direction |
| 12 | #4084 / #5993: workers unversioned by default | Cloud workers versioned (PINNED) with current-version automation | Approved in direction |
| 13 | Decision 005 R9: local-runner behaviours "have NO engine walk implementation and are not being ported" (`engineadmission/refusal.go:15-20`) | Reversed for all 8 refused features (P4) | **Approved (D-1)** |
| 14 | Mode 3 inside `goobers` (`goobers up` + `worker` + `engine-*`; the shared `goobers-system` base) | Frozen at P3 exit, deprecated one minor, removed in P6; the stage contract stays | **Approved (D-3)** |
| 15 | Per-run writer affinity (rejected in an earlier draft) | Adopted in P5 for redaction; no shared registry | D-5 default adopted |

**Not a departure:** cloud requires DSL ≥3.0 from its first release. That is dsl-3.0 D14 ("2.0 … local-runner-only") applied to cloud. **Draft amended:** the engine-child-workflows draft's phase order is reversed for parity ([§8.5](#engine-parity)); the draft is not an approved design.

---

<a id="alternatives"></a>
## 21. Alternatives considered

| Alternative | Verdict |
|---|---|
| **Thin daemon over Temporal** (cron as Schedules, claims as entity workflows, admission as a semaphore workflow) | Rejected for a **hybrid**. Admission is a global check-then-act over counters (`internal/localscheduler/conditions.go:90-143`), so in Temporal it would be a singleton entity workflow: a leader with worse ergonomics and a Continue-As-New hotspot. Policy churn in `localscheduler` would become determinism and versioning events. D0/D5 keep admission semantics out of Temporal history (#6447 removed Schedules). It removes no database, since the journal stays product-owned. Taken from it: per-run lifecycle in Temporal (HITL, claim liveness = workflow open, re-attach) |
| **Temporal Schedules for cron** | Rejected. A Schedule replaces only the timer (admission still runs in the controller), and the cursor CAS + source receipt already gives exactly-once acceptance without splitting the trigger source |
| **Azure SQL** | Rejected. Temporal does not support SQL Server (two engines, two backup stories); no LISTEN/NOTIFY; a larger dialect delta from SQLite (`ON CONFLICT`→`MERGE`, `RETURNING`→`OUTPUT`); no cloud-agnostic equivalent of CNPG |
| **Active-active scheduler with `SKIP LOCKED`** | Not now. Admission caps would become a distributed semaphore with a DB round trip per admit; demand and backlog polls would multiply against provider quota; pacing maps would need sharing. **Kept as the scale-out path:** if one controller saturates, shard by gaggle with bucket leases; the DB invariants already make that safe |
| **k8s Lease as the fence or as a hint** | Rejected. client-go does not guarantee a single active leader under pause or skew, and a k8s Lease can't be checked inside a DB transaction. As a hint it needs a k8s token and Lease RBAC on the controller for no gain over a ≤5s DB handover |
| **RWX / Azure Files and more replicas** | Rejected. flock and SQLite WAL are not dependable on RWX (#2854) |
| **Stop at P3** (state off RWO, single replica) | Delivers most user-visible value, but the PO committed to the end state, so P5/P6 are scheduled behind entry criteria rather than gated on demand |
| **Runner fallback in cloud** (in-controller or run-host pods) | Rejected by D-2 ([§8.6](#cloud-dsl-surface)) |
| **Wire `DispatchOne` for runner lanes** | Rejected: no runner gate arm, by design |
| **Keyed-fingerprint scrub registry** | Rejected ([§11.4](#scrubber)): can't be byte-identical; oracle or cost problems; availability coupling |
| **Hash-derived run IDs** | Rejected ([§10.1](#run-id-semantics)): changes reuse semantics; the acceptance row already exists |
| **One lease row with `FOR SHARE`** | Rejected ([§11.2](#lease)): fenced transactions block renew |
| **NOTIFY per emit** | Rejected ([§11.3](#journal-write-path)): database-wide commit serialization |
| **Temporal Cloud as the reference** | Supported option, not the reference ([§13.6](#temporal)) |
| **testcontainers-go for PG tests** | Rejected in the dependency record (heavy docker/moby tree). CI service containers over TCP with `//go:build integration` (the hermetic tier can't exec `pg_ctl` or `docker`) |

---

<a id="grounding"></a>
## Appendix A: Grounding

### A.1 Research basis

This design was built from six read-only research tracks against `main` @ `f42351fb1`, an adversarial review of the first draft, and the PO rulings of 2026-10-21:
- **A, control-plane coordination:** an inventory of 28 single-daemon mechanisms (condensed into C1–C8), the minimal leader set (L1 admission, L2 renew/reap, L3 config apply), and risks including non-excluding merge-lock domains and "not mine ⇒ dead" boot logic.
- **B, durable state and storage:** an inventory of 31 stores, journal-seam write-site counts, a Postgres schema sketch, Postgres vs Azure SQL, migration and backup policy, and the redaction-affinity risk.
- **C, Temporal tier:** the engine/worker map, worker drain behaviour, server HA and shard sizing, thin-daemon vs leader, and versioning rules (never reuse a patch ID; replay test per gate; names never change in place).
- **D, Kubernetes deployment:** the workload inventory against k8s practice (replicas, PDB, probes, drain, priority, spread), 15 blockers and manifest sketches.
- **E, external practice:** leases and fencing, CloudNativePG vs Azure PostgreSQL vs Azure SQL, Temporal production guidance, journals in SQL, and k8s rollout practice from public documentation and exemplar projects.
- **F, API/ingress and faults:** the request-surface inventory, auth and key sharing, idempotency and client retry windows, provider limits, and the fault model behind F1–F15.

The adversarial review (verdict: rethink) drove the current shape: the cloud DSL support surface, an honest local-impact claim, the missing work items and size, the start fence, the split lease rows, the journal counter and NOTIFY placement, the retry budget, and dropping the fingerprint scrub registry. The PO rulings set D-1…D-3. Reference-workflow usage in §8.1 was measured on `main` at the baseline.

### A.2 Key citations (all @ `f42351fb1`)

| Claim | Evidence |
|---|---|
| Singleton = flock `up.lock`; exit if held | `cmd/goobers/up.go:676-695`; `internal/platform/lock/lock_unix.go:11-24` |
| Preparer forces 1 replica / Recreate / 90s | `deploy/reference/authenticated/main.go:559-565` |
| Base api Deployment: no strategy, no grace | `deploy/reference/goobers-system/api-deployment.yaml:11-15` |
| Worker grace vs README "90 seconds" | `deploy/reference/goobers-system/worker-deployment.yaml:14-18,32`; `deploy/reference/README.md:771-773` |
| Temporal single replica; Burstable non-HA PG | `deploy/reference/temporal/values.yaml:30`; `infra/bicep/modules/postgres.bicep:18-19,49-51` |
| Engine refusal set and R9 "not being ported" | `internal/engineadmission/refusal.go:15-20,41-60,133-157,172-177,202-241,243` |
| Lanes refused by the engine stay on the runner; 2.0 lanes self-pin; child parents via `containedParentSelection` | `cmd/goobers/engineselection.go:60-130,145-150,240-250`; `internal/workflow/placement.go:46-55` |
| Runner→pod dispatch not wired | `internal/engine/dispatchone.go:14-30`; no production caller starts `DispatchOne` |
| Runner has no gate dispatch arm "and will not get one" | `docs/design/dsl-3.0.md:205-215` |
| 2.0 is local-runner-only | `docs/design/dsl-3.0.md` D14 |
| engine-child-workflows reverses R9 for 4 features (draft) | `docs/design/engine-child-workflows.md:15-18` |
| Feature registry has no runtime dimension | `internal/workflow/v_3_0/features.go:38-60` |
| Reference-workflow DSL versions and refused-feature usage | `reference-workflows/gaggles/goobers/workflows/*.yaml` (`quality-sprint.yaml:174`, `quality-spot-check.yaml:17,142,148`); `config-examples/gaggles/*/workflows/work-nomination.yaml` (`parallels:`) |
| `AlreadyStarted` ⇒ success; memo | `internal/engine/starter.go:126-131,133-135,138-140,157-170` |
| Acceptance ID = run ID; 7-day retention | `internal/triggerqueue/store.go:23-28,131-197`; `source_start.go:14-30,103-111`; `children.go:218`; `startintent/observe.go:21,43` |
| Schedule source key + cursor CAS | `internal/startintent/schedules.go:20-44`; `triggerqueue/source_start.go:120-124` |
| Worker observation keys | `internal/startintent/workers.go:21-53` |
| Remaining random minting | `internal/localscheduler/dispatchidentity.go:10-13`; `scheduler.go:228-229,2699-2701`; `childadmission.go:126`; `cmd/goobers/run_continue.go:168`; `internal/runner/child_quiescence.go:32` |
| planehttp defaults and classifier | `internal/planehttp/retry.go:42-48,183-203` |
| Artifact-record 3s deadline | `cmd/goobers/dispatchexec.go:1212-1221,1300-1301` |
| Grant key random per process | `cmd/goobers/credentialrefresh.go:59-60,80-95` |
| Outbox process-wide `sync.Map`, wall-clock lease | `internal/terminalreport/flight.go:8-25`; `queue.go:48-56` |
| Two merge-lock domains | `internal/claimsclient/file.go:153-162`; `internal/claimsclient/http.go:376-420` |
| Scrubber semantics and mint-time registration | `internal/journal/scrub.go:72-200`; `cmd/goobers/credentialrefresh.go:163,193`; `internal/githubapp/source.go:239`; `internal/credentials/capability.go:41`; `internal/childworkflow/grant_issuer.go:84`; workers use `journal.DefaultScrubber()` (`cmd/goobers/worker.go:504`, `workerwiring.go:231`) |
| Live writer liveness | `internal/livejournal/livejournal.go:553-567` |
| Stall sweep semantics | `cmd/goobers/stalledruns.go:24-59`; `docs/reference/workflow-primitives/graph-and-execution.md:436-444` |
| Stage activity options / retry loop; side-effecting stages never retry after worker loss | `internal/engine/engine.go:1345-1360`; `retry.go:105-120,286-288` |
| Worker drain 30s + 5m; pods deleted after; heartbeat = counter only | `internal/workerhost/workerhost.go:37-45`; `internal/dispatcher/child_custody.go:36-50`; `internal/engine/dispatchcancel.go:47-62` |
| PINNED registrations | `internal/engine/register.go:12-25` |
| Daemon `awaitInto` + terminal hooks | `cmd/goobers/enginestarter.go:240-263`; `cmd/goobers/engineterminalhooks.go:21-172` |
| "Not mine ⇒ dead" boot logic | `cmd/goobers/up.go:1370-1385,1665-1700`; `internal/localscheduler/renewal.go:41-45`; `internal/triggerqueue/store.go:234-259` |
| Admission check-then-act | `internal/localscheduler/conditions.go:90-143`; `scheduler.go:296-305` |
| Pod-token Registry vs SignedKey | `cmd/goobers/up.go:2668-2678`; `internal/podauth/podauth.go:144-155`; `internal/podauth/signed.go:17,43-95` |
| Read model refuses newer schema | `internal/readmodel/store.go:312-321` |
| No blob GC caller | only `Remove` caller: `internal/configgeneration/store.go:232` |
| Temporal CLI acquisition | `internal/temporaltest/devserver.go:14-21,40-63` |
| Gates exist | `test/cmdgoobersgrowth`, `test/architectureimports`, `test/deadcode`, `test/complexitygate`, `test/coveragegate`, `test/hermetic`, `test/nophonehome`, `test/ci/runtime-acquisitions.json` |
| Approved constraints | `docs/design/goobernetes-decisions.md` D0, D1, D5, D6, D11; `docs/design/distributed-state-and-coordination.md` DS1–DS10, §9, §10; `docs/design/goobernetes-architecture.md:33,54` and §4; `docs/design/k8s-infra-shape.md` §2, §4, §7; `docs/design/dsl-3.0.md` D6, D14 |
| DSL 3.0 lock state | #7590, #7609, #7616: open drafts awaiting the PO |

### A.3 P0 implementation notes (PRs open at 2026-10-21)

- PR #7689 sets the api pod grace to 3660s with an explicit `--drain-timeout 60m`, and the prepared worker grace to `DefaultDrainTimeout + ChildSettlementTimeout + 30s` (360s). It also corrects DS §10's Temporal Schedules text and documents the `RunScheduled` inverse as legacy, which completes HA-P0-09.
- HA-P0-08 (stage-boundary drain) replaces the interim 60-minute drain bound and shrinks the api grace to the new bound + 60s; it updates `TestDeployReferenceAPISingletonShutdownAndDisruption` accordingly.

### A.4 Still UNVERIFIED

- KEDA Temporal scaler; Worker Controller fit.
- Azure Disk detach time on node loss (today's RTO estimate).
- Entra token auth through PgBouncer 6432 with pgx.
- Azure on-demand backup per SKU and region.
- Azure PG GP-ZR cost per instance.
- AKS handling of bare pods with custody finalizers.
- Go SDK behaviour on unknown version markers in a mixed fleet.
- Temporal Cloud cross-cloud latency from Azure.
- The size of the crash-time redaction gap beyond pattern scrubbers (measured in HA-P5-03).
- The CI queue-time premise (re-measure).
