# Spec: Deployment & Infra

> Status: **Draft** · Derives from `../VISION.md` §4, §5 · Area prefix: `DEP`

## Purpose

Defines how an instance is provisioned and deployed, and how runs physically execute. The
goal is **simple infra** the team owns, driven entirely from the `infra` + `config` repos.

## Model

- **Infra:** an AKS cluster, storage + logging, the goober-run telemetry store, and disk —
  provisioned via **Bicep**.
- **Deploy (two-stage, GitOps):** a **release pipeline** applies the **`infra` repo**
  (Bicep) to bootstrap the cluster + a reconcile controller; that controller then watches
  the **`config` repo** and continuously reconciles the workforce's desired state
  (Helm-like counts + characteristics). `infra` runs rarely; `config` drives ongoing
  state.
- **A run = an ephemeral pod:** prepared with auth, the harness (signed in), and a fresh
  copy of the target repo; handed context + task via the invocation hook; signals
  completion via the completion tool; emits telemetry via management tools/hooks + log
  collection; then torn down.
- **Scaling** = replica counts driven by the scale factor in definitions.

## Requirements

### Provisioning & deploy
- **DEP-001 (MUST):** Instance infra MUST be provisioned via Bicep: AKS, storage, logging,
  goober-run telemetry store, and disk.
- **DEP-002 (MUST):** Bootstrap MUST run via a release pipeline applying the `infra` repo,
  configured with the team's connection info (self-hosted; no managed service).
- **DEP-003 (MUST):** A reconcile controller MUST continuously drive the `config` repo's
  manifest desired state into the running deployment idempotently (Helm-like / GitOps).
- **DEP-010 (MUST):** The `infra` and `config` repos MUST be deployable/permissioned
  independently (supports Tutor write-scoping — `SEC-021`).
- **DEP-009 (SHOULD):** Redeploy and rollback SHOULD be supported via the versioned infra
  repo.
- **DEP-011 (MUST):** The instance MUST run **Temporal self-hosted in-cluster** (OSS/MIT)
  as the workflow engine, backed by **Azure managed PostgreSQL** for persistence.
  Elasticsearch (advanced visibility) is deferred; basic visibility is sufficient for v1.
- **DEP-012 (MUST):** Reconciliation MUST use **ArgoCD** to sync `config`-repo definitions
  into the cluster as **CRDs**, plus a **Goobers operator** that reconciles those CRDs into
  running goober replicas and Temporal workflow registrations. The operator owns the
  domain reconcile; Argo owns git→cluster sync, drift detection, and rollback.

### Run execution
- **DEP-004 (MUST):** A run MUST execute as an ephemeral pod prepared with auth, the
  harness (signed in), and a fresh copy of the target repo (`GBO-011`).
- **DEP-005 (MUST):** The pod MUST receive context + task via the invocation hook
  (`GBO-012`) and the agent MUST signal completion via the completion tool (`GBO-013`).
- **DEP-006 (MUST):** Management tools/hooks + machine log collection MUST capture
  telemetry from each run (`GBO-020`, `GBO-021`).
- **DEP-007 (MUST):** Run pods MUST be torn down after completion (no lingering state).

### Scaling
- **DEP-008 (MUST):** Scaling a goober/workflow MUST be driven by the scale factor in its
  definition (change + redeploy → more replicas) (`GBO-030`).

## Relationships

- Provisions → the **Instance** infra and **Telemetry** store.
- Applies → the **Config-as-code** manifest.
- Hosts → **Workflow** engine and ephemeral **Goober** run pods.
- Bounded by → **Security** (per-gaggle namespaces/identities).

## Open questions

- **DEP-Q1:** Cold-start cost of a fresh repo copy per run — caching / sparse-checkout
  strategy?
- **DEP-Q2:** ~~Hosting the workflow engine~~ **Resolved:** Temporal self-hosted
  in-cluster (OSS/MIT, no license cost); persistence on **Azure managed PostgreSQL**;
  **Elasticsearch deferred** (basic visibility only for now). See `DEP-011`.
- **DEP-Q3:** Warm-pool / pre-warmed pods to reduce run latency.
- **DEP-Q4:** AKS resource sizing and autoscaling under load.
- **DEP-Q5:** ~~Build-vs-buy reconciliation tooling~~ **Resolved:** ArgoCD + Goobers
  operator (CRDs). See `DEP-012`.
