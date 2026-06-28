# Spec: Deployment & Infra

> Status: **Draft** · Derives from `../VISION.md` §4, §5 · Area prefix: `DEP`

## Purpose

Defines how an instance is provisioned and deployed, and how runs physically execute. The
goal is **simple infra** the team owns, driven entirely from the goober-infra repo.

## Model

- **Infra:** an AKS cluster, storage + logging, the goober-run telemetry store, and disk —
  provisioned via **Bicep**.
- **Deploy:** a **release pipeline** the team configures with connection info applies the
  goober-infra repo's desired state (Helm-like reconcile of counts + characteristics).
- **A run = an ephemeral pod:** prepared with auth, the harness (signed in), and a fresh
  copy of the target repo; handed context + task via the invocation hook; signals
  completion via the completion tool; emits telemetry via management tools/hooks + log
  collection; then torn down.
- **Scaling** = replica counts driven by the scale factor in definitions.

## Requirements

### Provisioning & deploy
- **DEP-001 (MUST):** Instance infra MUST be provisioned via Bicep: AKS, storage, logging,
  goober-run telemetry store, and disk.
- **DEP-002 (MUST):** Deployment MUST run via a release pipeline configured with the
  team's connection info (self-hosted; no managed service).
- **DEP-003 (MUST):** A deploy MUST reconcile the manifest's desired state into the running
  deployment idempotently (Helm-like).
- **DEP-009 (SHOULD):** Redeploy and rollback SHOULD be supported via the versioned infra
  repo.

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
- **DEP-Q2:** Hosting the workflow engine (Temporal) — in-cluster vs. managed.
- **DEP-Q3:** Warm-pool / pre-warmed pods to reduce run latency.
- **DEP-Q4:** AKS resource sizing and autoscaling under load.
- **DEP-Q5:** Build-vs-buy reconciliation tooling (Helm/GitOps like Flux/Argo vs. custom).
