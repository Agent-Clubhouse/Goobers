# Spec: Config-as-Code Model

> Status: **Draft** · Derives from `../VISION.md` §3, §4, §5 · Area prefix: `CFG`

## Purpose

Everything about a Goobers deployment — instances, gaggles, goobers, workflows, gates,
connections — is **defined and managed as code** in the goober-infra repo. This is the
control plane; the portal is only a window. This spec defines the model and conventions
for that code.

## Model

- The **goober-infra repo** holds: Bicep (infra), the goober codebase, and **agent/
  workflow definitions** — markdown + folders + a **YAML manifest** declaring desired
  state (Helm-like: counts + characteristics).
- A **deploy reconciles desired state** into the running deployment. Changing a scale
  factor and redeploying yields more replicas; adding a goober definition and deploying
  makes a new team member appear.
- **Setup and configuration are declarative and code-only.** No imperative/portal
  configuration. (Runtime *operations* — gate approvals, retries — are a separate,
  minimal portal surface; see Portal spec.)
- Changes flow through **git + PRs** — including the **Tutor**'s training PRs against
  goober definitions.

## Requirements

- **CFG-001 (MUST):** All configuration of instances, gaggles, goobers, and workflows MUST
  live as code in the goober-infra repo.
- **CFG-002 (MUST):** A YAML manifest MUST declare desired state — gaggles, goobers (with
  scale factors), workflows (triggers, tasks, gates, selectors), and connections.
- **CFG-003 (MUST):** Goober instructions/personas MUST be authored as markdown; skills
  and tools are referenced by the definition.
- **CFG-004 (MUST):** A deploy MUST reconcile declared desired state into the running
  deployment (idempotent apply).
- **CFG-005 (MUST):** Setup/config MUST be declarative; the platform MUST NOT require (or
  offer) UI-based configuration.
- **CFG-006 (MUST):** Changes MUST be versioned via git; PRs are the change mechanism,
  including Tutor-authored definition changes.
- **CFG-007 (SHOULD):** The repo SHOULD follow a documented reference folder layout so
  definitions are discoverable and composable.
- **CFG-008 (SHOULD):** Manifests SHOULD be validatable/lintable before deploy (catch
  bad definitions early).
- **CFG-009 (MUST):** Secrets MUST be referenced (not stored) in the repo (see Security).

## Relationships

- Defines → **Instance**, **Gaggle**, **Goober**, **Workflow**, **Task**, **Gate**.
- Applied by → the **Deployment** pipeline.
- Edited by → humans and the **Tutor** (via PRs).

## Open questions

- **CFG-Q1:** Concrete manifest schema for each primitive (the actual YAML shape).
- **CFG-Q2:** Reference folder layout for the infra repo.
- **CFG-Q3:** Definition composition/reuse mechanism (shared instruction/skill/stage
  fragments — `GBO-004`, `WF-005`).
- **CFG-Q4:** Secret reference mechanism (Key Vault refs?) — Security spec.
