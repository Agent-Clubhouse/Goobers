# Spec: Backlog & Providers

> Status: **Draft** · Aligned to ../ARCHITECTURE.md (2026-07-12) · Derives from ../VISION.md §5, §8 · Area prefix: `BL`

## Purpose

The **Backlog** is the external system of record for work items, and **Providers** are the
abstraction over the team's repo + backlog tooling (GitHub and ADO). This is how work
enters the system and how goobers act on code. The **GitHub provider is the V0 workload**
(`ARCHITECTURE.md §12`): the V0 workflows — backlog curation, work nomination,
implementation — run entirely on GitHub issues and PRs. **ADO lands in V1** behind the
same abstraction, whose shape is unchanged.

## Model

- **Provider abstraction over GitHub + ADO**, for both the **repo** and the **backlog**,
  from day one (`VISION §8`). GitHub ships first (V0); ADO follows (V1); definitions
  never change to switch provider.
- The **backlog is external** — a system of record the team already owns — not stored
  inside the instance (`ARCHITECTURE.md §2`).
- Work items are added by **humans or goobers** and carry **labels** used by the scheduler
  for routing (selectors). Items are **claimable** for exactly-once processing: the claim
  itself is runner-owned (lease/ledger); the provider carries a **claiming marker**
  (label/assignee) for human visibility and cross-run signaling.
- The **repo provider** supports the operations runs need: fresh clone/copy, branch, open
  PR, poll PR (review verdicts + CI status — driving the CI-poll/repass loop), close PR.

## Requirements

### Backlog

- **BL-001 (MUST):** *(All tiers)* The platform MUST support a backlog via a provider
  abstraction over GitHub and ADO. The GitHub provider is the **V0 workload**; the ADO
  provider is **V1** (`BL-033`). The abstraction's shape is fixed from day one.
- **BL-002 (MUST):** A **common work-item model** MUST map across providers (id, title,
  body, labels, state, assignee, links, optional parent-ref). The model is **flat for
  scheduling** — routing/claiming operate on individual items.
- **BL-012 (MUST):** Existing provider **hierarchy MUST be preserved as pass-through**
  (parent/child links carried untouched), so teams using epic→feature→PBI→task or flat
  issues both work. Hierarchy-**aware** behavior (rollup state, parent gating, auto-close)
  is **deferred** to a future iteration, not V0/V1.
- **BL-003 (MUST):** Work items MUST be addable by both humans and goobers, into the same
  backlog.
- **BL-004 (MUST):** Work items MUST carry labels that the scheduler matches against
  workflow selectors (`SCH-010`).
- **BL-005 (MUST):** Exactly-once processing is enforced instance-side by the **runner's
  lease-based claim** (`SCH-020`) — a claim ledger in instance state at tiers 1–2;
  **Tier 3 (V2):** Temporal workflow-id identity (one workflow per item id) — never in
  the backlog itself. The provider MUST, however, let us **write a claiming marker and
  status back** to an item (claimed/in-progress/done) for human visibility; the marker
  mirrors the claim, it is not its source of truth.
- **BL-006 (MUST):** The backlog MUST be treated as an external system of record —
  durable truth lives there, not in the instance.

### Repo provider

- **BL-010 (MUST):** A repo provider abstraction MUST support fresh copy/clone, branching,
  PR open/poll/close, and PR **review request/submit** (request a review; post a
  review verdict) across GitHub and ADO (used by runs and the Tutor). Review
  request/submit lands **V1** alongside ADO parity (`BL-033`); V0 ships open/poll/
  close (`BL-031`). At tiers 1–2 "fresh copy" is realized as a managed working copy +
  per-run worktrees (`DEP-026`); the provider contract is the same at every tier.
- **BL-011 (MUST):** Provider-native item types/states MUST be mapped to the common model
  (`BL-002`).

### V0 workload (GitHub) and V1 parity (ADO)

- **BL-030 (MUST):** *(V0)* The GitHub backlog provider MUST support: **read/query**
  issues (by label, state, assignee), **create**, **update**, **label/unlabel**, and
  **close** issues — the full surface the backlog-curation and work-nomination workflows
  need. Queries MUST support filtering on the eligibility/trust label so the
  untrusted-input gate (`SEC-047`) is enforceable at query time.
- **BL-031 (MUST):** *(V0)* The GitHub repo provider MUST support: **open** PRs, **poll**
  PRs for review verdicts and CI status (driving the implementation workflow's
  CI-poll/repass loop), and **close** PRs. (Review request/submit is V1 — `BL-010`.)
- **BL-032 (MUST):** *(V0)* The GitHub provider MUST apply and remove **claiming markers**
  (label and/or assignee) on items so concurrent runs observing the backlog never
  double-process (`WF-031`); the runner's claim ledger remains the claim source of truth
  (`BL-005`).
- **BL-033 (MUST):** *(V1)* The ADO provider MUST reach parity (work items + PRs +
  claiming markers) behind the same abstraction, with no change to workflow or goober
  definitions.

### Triggering

- **BL-020 (SHOULD):** New/changed backlog items SHOULD reach the scheduler promptly
  (webhook preferred, polling acceptable) to drive trigger evaluation. The local
  scheduler polls open items matching a backlog-item trigger's selector labels;
  each admitted run's first stage queries and claims a specific item
  (`ARCHITECTURE.md §7`).

## Relationships

- Read/claimed by → the **Scheduler** (routing) and the **runner** (lease-based claims).
- Written by → humans and **producer goobers** (curation, nomination).
- Acted on by → **Goobers** (repo provider: branches, PRs, CI polling).
- Modeled by → the **Config-as-code** connection settings in `instance.yaml` (which
  provider, which repo).

## Open questions

- **BL-Q1:** ~~Where claims/leases live~~ **Resolved (updated 2026-07-12):** claims are
  **runner-owned, lease-based** — a claim ledger in instance state at tiers 1–2, Temporal
  workflow-id identity at tier 3 — with the backlog item mirroring status via a
  provider-visible marker only (`ARCHITECTURE.md §7`, `SCH-Q5`, `BL-005`, `BL-032`).
- **BL-Q2:** *(build-time design)* Provider rate limits / API quotas under heavy gaggle
  load (client backoff + caching; respect quotas). Remains build-time design, per
  provider.
- **BL-Q3:** ~~Item model richness~~ **Resolved:** flat for scheduling; hierarchy
  preserved as pass-through; hierarchy-aware behavior deferred (`BL-002`, `BL-012`).
- **BL-Q4:** ~~Webhook vs. polling~~ **Resolved:** webhook-preferred with poll fallback,
  per provider (`BL-020`); the local scheduler's backlog-item trigger poll plus the
  query-and-claim first stage is the polling form. *(Remaining: per-provider auth
  specifics — build-time design.)*
