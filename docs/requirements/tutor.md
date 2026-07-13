# Spec: Tutor & Learning Loop

> Status: **Draft** · Aligned to `../ARCHITECTURE.md` (2026-07-12) · Derives from
> `../VISION.md` §4 (step 8), §8 · Area prefix: `TUT`

## Purpose

The **Tutor** closes the learning loop. It continuously analyzes the gaggle's own run
history, detects recurring patterns and failures, and **trains the workforce** by
opening PRs that improve definitions. This is the platform's core differentiator over "a
pool of coding agents."

The doctrine is unchanged by the tiered architecture: the Tutor is just a workflow, it
writes only to config, and humans hold the quality bar via PR controls. Only its data
source and the enforcement mechanism of its write-boundary vary by tier.

> **Roadmap note:** the Tutor is **V1 scope** (`ARCHITECTURE.md §12`) — it ships with
> teams/hardening, as a Tutor workflow if it needs more than the standard workflow
> primitives. Nothing about it is architecturally special; it waits only on the run
> history V0 accumulates.

## Model

- The Tutor is **itself a workflow** within the gaggle (same taxonomy), typically
  **schedule/signal-triggered** for continuous/periodic analysis.
- **Input:** the gaggle's own run history via the product contract — **run journals**
  (`ARCHITECTURE.md §4`) plus the **run-telemetry rollup store** (`telemetry.db` at
  tiers 1–2; ADX at tier 3): traces, prompts, gate outcomes, tool/script outputs
  across many runs. Never runner internals. *(All tiers)*
- **Detection:** recurring signals — the same tests failing, a reviewer repeating itself,
  a policy gate (e.g. coverage) repeatedly missed.
- **Output:** a **PR against the `config` repo/directory** proposing improvements.
- **Change scope (decided):** Tutor PRs may modify **goober definitions** (skills,
  instructions, tools) **and workflows and gates** — all of which live in `config`.
  Containment is **structural, not just policy**: the Tutor's identity can write to
  `config` only, so it *cannot* touch provisioning (`instance.yaml` / the tier-3
  `infra` repo / Bicep) or platform code. The boundary is enforced by
  **filesystem/repo permissions** at tiers 1–2 and **repo + identity permissions** at
  tier 3 (`SEC-021`, `ARCHITECTURE.md §9`).
- **Approval (decided):** the Tutor authors freely within `config`; humans control what
  merges via standard **`config`-repo governance** (branch protection / required review /
  CODEOWNERS, optionally path-scoped). No bespoke in-product gate or authoring restriction.

## Requirements

- **TUT-001 (MUST):** The Tutor MUST be modeled as a workflow within the gaggle.
  *(All tiers)*
- **TUT-002 (MUST):** The Tutor MUST be triggerable on a schedule/signal for continuous or
  periodic analysis.
- **TUT-003 (MUST):** The Tutor MUST detect issues via a **hybrid** approach:
  deterministic metric/threshold queries over the run-telemetry store (failure rates,
  repeated gate failures, retry counts — the local rollup store at tiers 1–2, ADX at
  tier 3) surface candidate problem areas, then an agentic step reads the relevant run
  journals/traces to diagnose and draft the change.
- **TUT-004 (MUST):** The Tutor MUST express improvements as a PR against the `config`
  repo/directory. *(All tiers)*
- **TUT-005 (MUST):** The Tutor's identity MUST have write access to `config` only —
  never provisioning (`instance.yaml`, the tier-3 `infra` repo) or platform code — so
  it structurally cannot change infra. Enforcement: filesystem/repo permissions at
  tiers 1–2; repo + identity permissions at tier 3 (`SEC-021`). *(All tiers)*
- **TUT-006 (MUST):** Within `config` the Tutor is **unrestricted in what it authors**
  (it may add, change, strengthen, or weaken goober definitions, workflows, and gates).
  Human control over the quality bar is exercised entirely through standard **`config`-repo
  governance** — branch protection, required reviews, and CODEOWNERS — not via in-product
  restrictions on the Tutor.
- **TUT-009 (SHOULD):** Setup SHOULD support **path-scoped governance** so teams can
  require stricter human review for sensitive changes (e.g. gate definitions) while
  allowing lower-risk changes (e.g. instruction tweaks) to flow more freely.
- **TUT-007 (MUST):** Tutor findings + rationale MUST be recorded to the run journal /
  telemetry store, and each proposed change SHOULD link to the evidence
  (journals/traces/patterns) that motivated it.
- **TUT-008 (SHOULD):** The Tutor SHOULD assess whether prior changes helped (closing the
  loop on its own edits) to avoid churn/regressions.
- **TUT-010 (MUST):** Detection MUST cover all four finding families — **failure
  patterns**, **waste** (duration/token/cost, retry waste), **gate noise** (gates that
  never fail, repetitive reviewer feedback), and **coverage gaps** (missing
  workflows/stages/tests) — per the detection catalog in
  `../design/v1/observability-substrate.md` (D7). The waste family depends on usage
  accounting (`TEL-041`).
- **TUT-011 (SHOULD):** Tutor proposals SHOULD span the full config surface, e.g.:
  adding test or gate stages; changing a goober's skills, instructions, stage
  prompts (`goal`), or **model** (requires the `Goober.spec.model` field — design
  D9); adding or removing entire workflows to cover gaps; removing or loosening
  noisy gates. The Tutor also reads the declarative workflow/goober definitions
  themselves as input (`config` read access) — evidence-linked per `TUT-007`.

## Relationships

- Is a → **Workflow** (special-purpose). **V1 scope** per `ARCHITECTURE.md §12`.
- Reads → **run journals** + the **Telemetry** run store (rollup at tiers 1–2, ADX at
  tier 3) — the same contract the portal reads; never runner internals.
- Writes → PRs via the repo provider against **Goober/Workflow/Gate** definitions in
  `config`.
- Bounded by → the **Security** write-boundary (`SEC-021`): filesystem/repo
  permissions locally, repo + identity permissions at tier 3.
- Gated by → standard `config` PR governance (human review as configured).

## Open questions

- **TUT-Q1:** ~~Detection method~~ **Resolved:** hybrid — metrics surface candidates,
  agentic step diagnoses & drafts (`TUT-003`). Re-anchored on the tiered stores: local
  rollup at tiers 1–2, ADX at tier 3 (`ARCHITECTURE.md §8`).
- **TUT-Q2:** ~~Can the Tutor weaken/remove a required gate?~~ **Resolved:** yes — the
  Tutor authors freely; humans hold the quality bar through `config`-repo PR governance
  (branch protection / required review / CODEOWNERS), not via in-product restrictions
  (`TUT-006`, `TUT-009`).
- **TUT-Q3:** **Resolved (default):** one PR per finding (batchable). *(Build-time:
  batching heuristics.)*
- **TUT-Q4:** **Resolved (default):** track prior-change history + assess whether they
  helped (`TUT-008`) to avoid flip-flopping a definition. *(Build-time: detection.)*
