# Spec: Backlog & Providers

> Status: **Draft** · Derives from `../VISION.md` §5, §8 + routing/claiming decisions · Area prefix: `BL`

## Purpose

The **Backlog** is the external system of record for work items, and **Providers** are the
abstraction over the team's repo + backlog tooling (GitHub and ADO). This is how work
enters the system and how goobers act on code.

## Model

- **Provider abstraction over GitHub + ADO**, for both the **repo** and the **backlog**,
  from day one (`VISION §8`).
- The **backlog is external** — a system of record the team already owns — not stored
  inside the instance.
- Work items are added by **humans or goobers** and carry **labels** used by the scheduler
  for routing (selectors). Items are **claimable** for exactly-once processing.
- The **repo provider** supports the operations runs need: fresh clone/copy, branch, open
  PR, request/review.

## Requirements

### Backlog
- **BL-001 (MUST):** The platform MUST support a backlog via a provider abstraction over
  GitHub and ADO.
- **BL-002 (MUST):** A **common work-item model** MUST map across providers (id, title,
  body, labels, state, assignee, links, optional parent-ref). The model is **flat for
  scheduling** — routing/claiming operate on individual items.
- **BL-012 (MUST):** Existing provider **hierarchy MUST be preserved as pass-through**
  (parent/child links carried untouched), so teams using epic→feature→PBI→task or flat
  issues both work. Hierarchy-**aware** behavior (rollup state, parent gating, auto-close)
  is **deferred** to a future iteration, not v1.
- **BL-003 (MUST):** Work items MUST be addable by both humans and goobers, into the same
  backlog.
- **BL-004 (MUST):** Work items MUST carry labels that the scheduler matches against
  workflow selectors (`SCH-010`).
- **BL-005 (MUST):** Exactly-once processing is enforced instance-side via Temporal
  workflow identity (`SCH-020`), not in the backlog. The provider MUST, however, let us
  **write status back** to an item (claimed/in-progress/done) for human visibility.
- **BL-006 (MUST):** The backlog MUST be treated as an external system of record — durable
  truth lives there, not in the instance.

### Repo provider
- **BL-010 (MUST):** A repo provider abstraction MUST support fresh copy/clone, branching,
  and PR open/review across GitHub and ADO (used by runs and the Tutor).
- **BL-011 (MUST):** Provider-native item types/states MUST be mapped to the common model
  (`BL-002`).

### Triggering
- **BL-020 (SHOULD):** New/changed backlog items SHOULD reach the scheduler promptly
  (webhook preferred, polling acceptable) to drive trigger evaluation.

## Relationships

- Read/claimed by → the **Scheduler** (routing + claiming).
- Written by → humans and **producer goobers**.
- Acted on by → **Goobers** (repo provider: branches, PRs).
- Modeled by → the **Config-as-code** connection settings (which provider, which repo).

## Open questions

- **BL-Q1:** ~~Where claims/leases live~~ **Resolved:** instance-side via Temporal
  identity; backlog item mirrors status only (`SCH-Q5`).
- **BL-Q2:** Provider rate limits / API quotas under heavy gaggle load.
- **BL-Q3:** ~~Item model richness~~ **Resolved:** flat for scheduling; hierarchy
  preserved as pass-through; hierarchy-aware behavior deferred (`BL-002`, `BL-012`).
- **BL-Q4:** ~~Webhook vs. polling~~ **Resolved:** webhook-preferred with poll fallback,
  per provider (`BL-020`). *(Remaining: per-provider auth specifics.)*
