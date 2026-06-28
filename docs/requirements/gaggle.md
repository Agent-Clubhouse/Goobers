# Spec: Gaggle

> Status: **Draft** · Derives from `../VISION.md` §6 · Area prefix: `GAG`

## Purpose

A **Gaggle** is a siloed workforce of goobers within an instance. It is the unit of
isolation and the container for a coordinated set of goobers and workflows pointed at a
target.

## Model

- An instance has **many gaggles**; each is **siloed** from the others.
- A gaggle contains its own **goobers** and **workflows**.
- A gaggle targets a **project codebase** and a **backlog**. The **backlog is a singleton
  per gaggle** (its single source of work-item truth).
- "Siloed" implies an isolation boundary for credentials, secrets, work, and telemetry
  scope (details owned by the Security spec).

## Requirements

- **GAG-001 (MUST):** A Gaggle MUST be a siloed workforce within an instance.
- **GAG-002 (MUST):** An instance MUST support multiple gaggles.
- **GAG-003 (MUST):** A Gaggle MUST contain its own goobers and workflows.
- **GAG-004 (MUST):** A Gaggle MUST target a project codebase and a backlog; the backlog
  MUST be a singleton for that gaggle.
- **GAG-005 (MUST):** Gaggles MUST be isolated from one another — secrets, credentials,
  work, and telemetry scoping MUST NOT leak across gaggles (details in Security spec).
- **GAG-006 (MUST):** A Gaggle MUST be defined as code in the goober-infra repo.
- **GAG-007 (COULD):** A Gaggle COULD target multiple repos / telemetry sources (less
  standard setup), while the backlog and goober-infra remain singletons.

## Relationships

- Belongs to → an **Instance**.
- Contains → **Goobers** and **Workflows**.
- Targets → a project **repo** + a **Backlog** (singleton).
- Scopes → its slice of the **Telemetry** store.

## Open questions

- **GAG-Q1:** Exact isolation guarantees and mechanism (namespace per gaggle? separate
  identities/secrets?) — Security spec.
- **GAG-Q2:** Can goobers/workflows be shared/templated across gaggles, or are they always
  gaggle-local definitions?
