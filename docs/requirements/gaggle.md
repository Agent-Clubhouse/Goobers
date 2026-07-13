# Spec: Gaggle

> Status: **Draft** · Aligned to `../ARCHITECTURE.md` (2026-07-12) · Derives from
> `../VISION.md` §6 · Area prefix: `GAG`

## Purpose

A **Gaggle** is a siloed workforce of goobers within an instance. It is the unit of
isolation and the container for a coordinated set of goobers and workflows pointed at a
target — the same unit whether the instance is one binary on a laptop or a cluster over
a monorepo.

## Model

- An instance has **many gaggles**; each is **siloed** from the others.
- A gaggle contains its own **goobers** and **workflows**.
- A gaggle targets a **project codebase** and a **backlog**. The **backlog is a singleton
  per gaggle** (its single source of work-item truth).
- "Siloed" implies an isolation boundary for credentials, secrets, work, and telemetry
  scope. Isolation is a **ladder by tier** (`ARCHITECTURE.md §9`): per-gaggle scoping
  within the instance (working copies, run journals, telemetry, credential resolution)
  at tiers 1–2; Kubernetes namespaces + identities + network policy at tier 3
  (details owned by the Security spec).
- A gaggle definition is **tier-portable**: the same definition runs unchanged on the
  local runner (tiers 1–2) and the Temporal runner (tier 3), producing semantically
  identical run journals (`ARCHITECTURE.md §3.3`).

## Requirements

- **GAG-001 (MUST):** A Gaggle MUST be a siloed workforce within an instance.
  *(All tiers)*
- **GAG-002 (MUST):** An instance MUST support multiple gaggles. *(All tiers)*
- **GAG-003 (MUST):** A Gaggle MUST contain its own goobers and workflows. *(All tiers)*
- **GAG-004 (MUST):** A Gaggle MUST target a project codebase and a backlog; the backlog
  MUST be a singleton for that gaggle. *(All tiers)*
- **GAG-005 (MUST):** Gaggles MUST be isolated from one another — secrets, credentials,
  work, and telemetry scoping MUST NOT leak across gaggles. Enforcement follows the
  tier isolation ladder (details in Security spec). *(All tiers)*
- **GAG-006 (MUST):** A Gaggle MUST be defined as code in the `config` repo/directory.
  *(All tiers)*
- **GAG-007 (COULD):** A Gaggle COULD target multiple repos / telemetry sources (less
  standard setup), while the backlog, provisioning source (`instance.yaml` at tiers
  1–2 / `infra` repo at tier 3), and `config` repo/directory remain singletons.
- **GAG-010 (MUST):** A Gaggle definition MUST be tier-portable — the same definition,
  unmodified, MUST run on both the local runner and the Temporal runner; scaling a
  gaggle to tier 3 MUST NOT require rewriting any part of the workforce. *(All tiers)*
- **GAG-011 (MUST):** At tiers 1–2 gaggle isolation MUST be enforced by per-gaggle
  scoping inside the instance root: separate managed working copies/worktrees, run
  journals attributed per gaggle, telemetry rows partitioned per gaggle, and gaggle-
  scoped credential resolution. *(Tiers 1–2)*
- **GAG-012 (SHOULD):** **Tier 3 (V2):** at cloud scale each gaggle SHOULD map to its
  own Kubernetes namespace, workload identity, and telemetry partition — the cloud
  drop-in for the same isolation seam `GAG-011` implements locally. *(Tier 3, V2)*

## Relationships

- Belongs to → an **Instance**.
- Contains → **Goobers** and **Workflows**.
- Targets → a project **repo** + a **Backlog** (singleton).
- Scopes → its slice of the run journals and the **Telemetry** store.

## Open questions

- **GAG-Q1:** **Resolved (updated 2026-07-12):** isolation is a **ladder by tier** —
  per-gaggle scoping within the instance at tiers 1–2 (`GAG-011`); namespace +
  identity + secrets per gaggle at tier 3 (`GAG-012`, `SEC-001/002`,
  `ARCHITECTURE.md §9`). *(Previously resolved as namespace-only; the tier ladder
  generalizes it.)*
- **GAG-Q2:** **Resolved (default):** definitions are gaggle-local but MAY be shared via
  `config`-repo fragments/templates (`CFG-Q3`). *(Build-time: composition mechanics.)*
