# Spec: Deployment & Infra

> Status: **Draft** · Aligned to ../ARCHITECTURE.md (2026-07-12) · Derives from ../VISION.md §4, §5 · Area prefix: `DEP`

## Purpose

Defines how an instance is installed or provisioned, how the daemon/runner lifecycle
works, and how runs physically execute — at every tier. Goobers is **one system across
three deployment tiers** (`ARCHITECTURE.md §1`): tiers 1–2 run the **local runner** — a
single Go binary with files as durable state — and ship first (V0/V1); tier 3 drops the
**Temporal runner** + AKS + GitOps in behind the same seam (V2). The tier is a deployment
choice, never a product fork: the same definitions run everywhere.

## Model

- **Tiers 1–2 (local runner, ships first):** install one binary (`goobers`); `goobers
  init` scaffolds the instance layout (`ARCHITECTURE.md §6`); `goobers up` runs the daemon
  (embedded scheduler + runner). No database, no message bus, no service cluster.
  Durability is the file-based, append-only run journal; crash recovery is journal replay.
  "Infra" collapses to the binary + `instance.yaml`.
- **Tier 3 (V2, cloud drop-in):** an AKS cluster, storage + logging, the goober-run
  telemetry store, and disk — provisioned via **Bicep** from a real `infra` repo;
  **ArgoCD** + a **Goobers operator** deliver `config` as CRDs; **Temporal** (self-hosted,
  Postgres-backed) hosts the same compiled workflows and projects its history down into
  the same run-journal format (`ARCHITECTURE.md §3.2, §10`).
- **A run executes in an ephemeral, isolated run environment** handed goal + context via
  the **invocation envelope**; finishing via the **result envelope**; emitting telemetry;
  then torn down. Repo-backed stages are prepared with auth, the harness where needed,
  and a fresh working copy of the target repo. Deterministic stages may instead use an
  empty scratch workspace. Tiers 1–2: a git worktree or scratch directory + local
  process. Tier 3: an ephemeral pod. Same contract, different substrate.
- **Scaling** = replica counts driven by the scale factor in definitions — concurrent
  local runs at tiers 1–2, pod/worker replicas at tier 3.

## Requirements

### Tier 1–2 deployment (local runner, V0/V1)

- **DEP-020 (MUST):** *(Tiers 1–2)* The platform MUST ship as a **single Go binary**
  (`goobers`) with no required service dependencies — no database, message bus, or
  cluster. Installation is placing the binary on the machine.
- **DEP-021 (MUST):** *(Tiers 1–2)* `goobers init` MUST scaffold the instance layout per
  `ARCHITECTURE.md §6`: `instance.yaml` (connections: target repo(s), provider, token
  refs, telemetry settings), `config/`, `runs/`, `scheduler/`, `telemetry.db`, and
  `workcopies/`. Owning statement: `INST-010`; this ID defers to it.
- **DEP-022 (MUST):** *(Tiers 1–2)* `goobers validate` MUST check the instance and all
  definitions before anything runs; unvalidated or invalid definitions fail closed.
  Owning statement: `INST-012`; this ID defers to it.
- **DEP-023 (MUST):** *(Tiers 1–2)* `goobers up` MUST run the daemon (embedded scheduler +
  local runner); `goobers status` and `goobers trace <run-id>` MUST inspect the instance
  and its run journals without stopping it. Owning statement: `INST-012`; this ID defers
  to it.
- **DEP-024 (MUST):** *(Tiers 1–2)* Durability MUST be file-based: append + fsync on the
  event journal and atomically-replaced `state.json` checkpoints. After a crash, restart
  MUST recover by replaying `state.json` + the journal and resuming each run from its
  last completed stage (`ARCHITECTURE.md §3.1`). Owning requirement: `WF-054`; this ID
  defers to it.
- **DEP-025 (MUST):** *(Tiers 1–2)* The daemon MUST watch the local `config/` directory
  and apply validated definition changes as the tier 1–2 config-delivery mechanism
  (tier-3 counterpart: `DEP-012`). Version pinning holds: in-flight runs complete on the
  definition version they started with (`WF-016`). Owning statement: `CFG-020`; this ID
  defers to it.
- **DEP-026 (MUST):** *(Tiers 1–2)* Target repos MUST be materialized as **managed working
  copies** under `workcopies/`, separate from any working copy the user edits; per-run
  stage worktrees branch off these (`DEP-004`).
- **DEP-027 (SHOULD):** *(Tier 2)* The same binary SHOULD run as a long-lived daemon on a
  workstation, shared box, or small cloud VM/container. Tier 2 is an operational posture
  of tier 1, not a different build.

### Tier 3 deployment (cloud drop-ins, V2)

These requirements are the drop-in specs for scaling an instance to tier 3
(`ARCHITECTURE.md §10, §12`). They are retained, not deleted: this is where each cloud
component goes when you outgrow the box.

- **DEP-001 (MUST):** **Tier 3 (V2):** Instance infra MUST be provisioned via Bicep: AKS,
  storage, logging, goober-run telemetry store, and disk.
- **DEP-002 (MUST):** **Tier 3 (V2):** Bootstrap MUST run via a release pipeline applying
  the `infra` repo, configured with the team's connection info (self-hosted; no managed
  service).
- **DEP-003 (MUST):** **Tier 3 (V2):** A reconcile controller MUST continuously drive the
  `config` repo's manifest desired state into the running deployment idempotently
  (Helm-like / GitOps). Tier 1–2 counterpart: the daemon's `config/` watch (`DEP-025`).
- **DEP-010 (MUST):** **Tier 3 (V2):** The `infra` and `config` repos MUST be
  deployable/permissioned independently (supports Tutor write-scoping — `SEC-021`). At
  tiers 1–2 the same boundary holds with "infra" collapsed to the binary +
  `instance.yaml`: the provisioning surface is never writable via `config/`.
- **DEP-009 (SHOULD):** **Tier 3 (V2):** Redeploy and rollback SHOULD be supported via the
  versioned infra repo.
- **DEP-011 (MUST):** **Tier 3 (V2):** The instance MUST run **Temporal self-hosted
  in-cluster** (OSS/MIT) as the tier-3 runner behind the runner seam, backed by **Azure
  managed PostgreSQL** for persistence. The runner MUST project Temporal history down
  into the same run-journal format (`ARCHITECTURE.md §3.2, §4`) so the portal, telemetry,
  Tutor, and operators see one shape at every tier; raw Temporal mechanics are never the
  product surface. Elasticsearch (advanced visibility) is deferred; basic visibility is
  sufficient.
- **DEP-012 (MUST):** **Tier 3 (V2):** Reconciliation MUST use **ArgoCD** to sync
  `config`-repo definitions into the cluster as **CRDs**, plus a **Goobers operator**
  that reconciles those CRDs into running goober replicas and Temporal workflow
  registrations. The operator owns the domain reconcile; Argo owns git→cluster sync,
  drift detection, and rollback.

### Run execution (runner-seam contracts)

These are seam contracts, satisfied by both runners; the pod wording is the tier-3 form.

- **DEP-004 (MUST):** *(All tiers)* A run MUST execute in an **ephemeral, isolated run
  environment**. Repo-backed stages MUST receive a fresh working copy of the target repo
  and auth plus the signed-in harness where required (`GBO-011`); deterministic stages
  MAY instead receive an empty scratch workspace (`TSK-040`). Tiers 1–2: an isolated git
  worktree branched off the managed working copy (`DEP-026`) or a scratch directory,
  plus a local process. **Tier 3 (V2):** an ephemeral Kubernetes pod.
- **DEP-005 (MUST):** *(All tiers)* The run environment MUST receive goal + context via
  the **invocation envelope** (`GBO-012`) and the agent MUST signal completion via the
  **result envelope** (`GBO-013`). The envelope schemas are identical at every tier.
- **DEP-006 (MUST):** *(All tiers)* Management tools/hooks + machine log collection MUST
  capture telemetry from each run (`GBO-020`, `GBO-021`), landing as journal spans + the
  tier's run-telemetry store (`TEL-010`).
- **DEP-007 (MUST):** *(All tiers)* Run environments MUST be torn down after completion —
  worktrees and scratch directories removed at tiers 1–2, pods deleted at tier 3. No
  lingering state; the run journal is the durable record.

### Scaling

- **DEP-008 (MUST):** *(All tiers)* Scaling a goober/workflow MUST be driven by the scale
  factor in its definition (`GBO-030`). Tiers 1–2: N concurrent runs drawing from shared
  claimed work under the scheduler's max-parallel conditions. **Tier 3 (V2):** pod/worker
  replica counts (change + redeploy → more replicas).

## Relationships

- Installs/provisions → the **Instance** and the **Telemetry** store.
- Applies → the **Config-as-code** definitions (daemon watch at tiers 1–2; ArgoCD +
  operator at tier 3).
- Hosts → the **runner** (local runner at tiers 1–2; Temporal runner at tier 3) and
  ephemeral **Goober** run environments.
- Bounded by → **Security** (worktree/process isolation locally; per-gaggle
  namespaces/identities at tier 3).

## Open questions

- **DEP-Q1:** *(build-time design, tier 3)* Cold-start cost of a fresh repo copy per run —
  caching / sparse-checkout strategy. *(Tiers 1–2 largely avoid this: worktrees branch
  off the managed working copy.)*
- **DEP-Q2:** ~~Hosting the workflow engine~~ **Resolved (updated 2026-07-12):** two
  runners behind one seam per `ARCHITECTURE.md §3` — the **local runner** (single binary,
  file journal) ships first for tiers 1–2; **Temporal self-hosted in-cluster** (OSS/MIT,
  Azure managed PostgreSQL, Elasticsearch deferred) is the tier-3 drop-in with history
  projected into the run-journal format. See `DEP-011`, `DEP-024`.
- **DEP-Q3:** *(build-time design, tier 3)* Warm-pool / pre-warmed pods to reduce run
  latency.
- **DEP-Q4:** *(build-time design, tier 3)* AKS resource sizing and autoscaling (cluster
  autoscaler + HPA) under load.
- **DEP-Q5:** ~~Build-vs-buy reconciliation tooling~~ **Resolved:** ArgoCD + Goobers
  operator (CRDs) as the tier-3 config delivery; at tiers 1–2 delivery is the daemon
  watching `config/` (`ARCHITECTURE.md §10`). See `DEP-012`, `DEP-025`.
- **DEP-Q6:** *(build-time design)* Daemon supervision at tiers 1–2 (systemd/launchd/
  service wrapper), packaging and install channels (V1 packaging story).
