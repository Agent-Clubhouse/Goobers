# Spec: Config-as-Code Model

> Status: **Draft** · Derives from `../VISION.md` §3, §4, §5 · Area prefix: `CFG`

## Purpose

Everything about a Goobers deployment — instances, gaggles, goobers, workflows, gates,
connections — is **defined and managed as code** in the `infra` + `config` repos. This is the
control plane; the portal is only a window. This spec defines the model and conventions
for that code.

## Model

- Config lives in **two repos**, split by cadence and blast radius:
  - **`infra` repo** — Bicep / cluster bootstrap / connection wiring. Rarely changes.
  - **`config` repo** — the workforce as code: **agent/workflow/gate definitions**
    (markdown + folders) + a **YAML manifest** declaring desired state (Helm-like: counts
    + characteristics). Changes constantly; **the only repo the Tutor writes to**.
  - (Platform/engine code is upstream, consumed as images/charts — not a per-instance
    repo.)
- A **deploy/reconcile drives desired state** into the running deployment. Changing a
  scale factor and reconciling yields more replicas; adding a goober definition makes a
  new team member appear.
- **Setup and configuration are declarative and code-only.** No imperative/portal
  configuration. (Runtime *operations* — gate approvals, retries — are a separate,
  minimal portal surface; see Portal spec.)
- Changes flow through **git + PRs** — including the **Tutor**'s training PRs against the
  `config` repo.

## Requirements

- **CFG-001 (MUST):** All configuration MUST live as code: infra/bootstrap in the `infra`
  repo; gaggle/goober/workflow/gate definitions + manifest in the `config` repo.
- **CFG-010 (MUST):** The `infra` and `config` repos MUST be separable by access control,
  so the Tutor's identity can be granted write to `config` only (structural containment —
  see `TUT-005`, `SEC-021`).
- **CFG-002 (MUST):** A YAML manifest MUST declare desired state — gaggles, goobers (with
  scale factors), workflows (triggers, tasks, gates, selectors), and connections.
- **CFG-003 (MUST):** Goober instructions/personas MUST be authored as markdown; skills
  and tools are referenced by the definition.
- **CFG-004 (MUST):** A deploy MUST reconcile declared desired state into the running
  deployment (idempotent apply).
- **CFG-005 (MUST):** Setup/config MUST be declarative; the platform MUST NOT require (or
  offer) UI-based configuration.
- **CFG-006 (MUST):** Changes MUST be versioned via git; PRs are the change mechanism,
  including Tutor-authored `config`-repo changes.
- **CFG-007 (SHOULD):** Each repo SHOULD follow a documented reference folder layout so
  definitions are discoverable and composable.
- **CFG-008 (SHOULD):** Manifests SHOULD be validatable/lintable before deploy (catch
  bad definitions early).
- **CFG-009 (MUST):** Secrets MUST be referenced (not stored) in the repo (see Security).

## Relationships

- Defines → **Instance**, **Gaggle**, **Goober**, **Workflow**, **Task**, **Gate**.
- Applied by → the **Deployment** pipeline.
- Edited by → humans and the **Tutor** (via PRs).

## Open questions

- **CFG-Q1:** *(build-time design)* Concrete manifest schema for each primitive (the
  actual YAML/CRD shape).
- **CFG-Q2:** *(build-time design)* Reference folder layout for the `infra`/`config` repos.
- **CFG-Q3:** *(build-time design)* Definition composition/reuse mechanism (shared
  fragments — `GBO-004`, `WF-005`).
- **CFG-Q4:** **Resolved:** secrets are **Key Vault references** (never stored in repo) —
  `SEC-010`, `CFG-009`.
- **CFG-Q5:** *(build-time design)* Version compatibility between the upstream platform
  release and the `config` repo's definitions (operator checks compatibility).
