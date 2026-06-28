# Spec: Instance / Tenant

> Status: **Draft** · Derives from `../VISION.md` §3, §4, §5 · Area prefix: `INST`

## Purpose

An **Instance** (Tenant) is a deployed Goobers installation — the top-level boundary that
owns shared infrastructure and hosts one or more gaggles. It is what a team stands up,
owns, and operates.

## Model

- An instance is **bootstrapped from the `infra` repo via a release pipeline** the team
  configures with their connection info — it is self-hosted, not a managed service.
- It provisions **simple infra**: an AKS cluster, storage + logging, the goober-run
  telemetry store, and disk.
- It is **bring-your-own**: the team supplies Azure, the agent harness (Copilot), project
  repo(s), and a backlog.
- It hosts **many gaggles** (siloed workforces).
- The Goobers side of an instance is **two singleton repos** (see Config-as-code spec):
  an **`infra` repo** (Bicep/bootstrap, rarely changes) and a **`config` repo** (the
  workforce as code, changes constantly, Tutor-writable). The platform/engine code is
  upstream (consumed as images/charts), not a per-instance repo.

## Requirements

- **INST-001 (MUST):** An instance MUST be deployable from the `infra` repo via a
  release pipeline configured with the team's connection info (no managed service).
- **INST-002 (MUST):** On first boot the instance MUST serve the portal showing an
  "I'm alive" / ready state with no gaggles/goobers configured yet.
- **INST-003 (MUST):** An instance MUST provision/own shared infra: AKS, storage, logging,
  and the goober-run telemetry store.
- **INST-004 (MUST):** An instance MUST host one or more gaggles.
- **INST-005 (MUST):** An instance MUST integrate bring-your-own subscriptions/resources:
  Azure, the agent harness, project repo(s), and a backlog.
- **INST-006 (MUST):** All instance configuration MUST be code (no UI configuration);
  see Config-as-code spec.
- **INST-007 (MUST):** The `infra` repo and the `config` repo MUST each be a singleton
  per instance.
- **INST-008 (SHOULD):** An instance SHOULD be reproducible/redeployable from the infra
  repo alone (idempotent apply of desired state).

## Relationships

- Deployed from → the **`infra` repo** (bootstrap) + **`config` repo** (reconciled
  desired state) via release pipeline / controller (Deployment spec).
- Hosts → many **Gaggles**.
- Owns → shared infra and the **Telemetry** store.
- Surfaced by → the **Portal**.

## Open questions

- **INST-Q1:** Multi-instance / multi-environment story (dev vs. prod instances)?
- **INST-Q2:** What exactly is shared at the instance level vs. isolated per gaggle
  (telemetry store shared with per-gaggle scoping? networking?) — coordinate with
  Security.
