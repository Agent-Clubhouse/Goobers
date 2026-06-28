# Spec: Tutor & Learning Loop

> Status: **Draft** · Derives from `../VISION.md` §4 (step 8), §8 · Area prefix: `TUT`

## Purpose

The **Tutor** closes the learning loop. It continuously analyzes the gaggle's own
telemetry, detects recurring patterns and failures, and **trains the workforce** by
opening PRs that improve definitions. This is the platform's core differentiator over "a
pool of coding agents."

## Model

- The Tutor is **itself a workflow** within the gaggle (same taxonomy), typically
  **schedule/signal-triggered** for continuous/periodic analysis.
- **Input:** the goober-run telemetry store — traces, prompts, gate outcomes, tool/script
  outputs across many runs.
- **Detection:** recurring signals — the same tests failing, a reviewer repeating itself,
  a policy gate (e.g. coverage) repeatedly missed.
- **Output:** a **PR against the `config` repo** proposing improvements.
- **Change scope (decided):** Tutor PRs may modify **goober definitions** (skills,
  instructions, tools) **and workflows and gates** — all of which live in the `config`
  repo. Containment is **structural, not just policy**: the Tutor's identity has write
  access to `config` only, so it *cannot* touch the `infra` repo / Bicep / platform code.
- **Approval (decided):** the Tutor authors freely within `config`; humans control what
  merges via standard **`config`-repo governance** (branch protection / required review /
  CODEOWNERS, optionally path-scoped). No bespoke in-product gate or authoring restriction.

## Requirements

- **TUT-001 (MUST):** The Tutor MUST be modeled as a workflow within the gaggle.
- **TUT-002 (MUST):** The Tutor MUST be triggerable on a schedule/signal for continuous or
  periodic analysis.
- **TUT-003 (MUST):** The Tutor MUST detect issues via a **hybrid** approach: deterministic
  metric/threshold queries over ADX/OTel telemetry (failure rates, repeated gate failures,
  retry counts) surface candidate problem areas, then an agentic step reads the relevant
  traces to diagnose and draft the change.
- **TUT-004 (MUST):** The Tutor MUST express improvements as a PR against the `config`
  repo.
- **TUT-005 (MUST):** The Tutor's identity MUST have write access to the `config` repo
  only — never the `infra` repo — so it structurally cannot change platform/infra.
- **TUT-006 (MUST):** Within `config` the Tutor is **unrestricted in what it authors**
  (it may add, change, strengthen, or weaken goober definitions, workflows, and gates).
  Human control over the quality bar is exercised entirely through standard **`config`-repo
  governance** — branch protection, required reviews, and CODEOWNERS — not via in-product
  restrictions on the Tutor.
- **TUT-009 (SHOULD):** Setup SHOULD support **path-scoped governance** so teams can
  require stricter human review for sensitive changes (e.g. gate definitions) while
  allowing lower-risk changes (e.g. instruction tweaks) to flow more freely.
- **TUT-007 (MUST):** Tutor findings + rationale MUST be recorded to telemetry, and each
  proposed change SHOULD link to the evidence (traces/patterns) that motivated it.
- **TUT-008 (SHOULD):** The Tutor SHOULD assess whether prior changes helped (closing the
  loop on its own edits) to avoid churn/regressions.

## Relationships

- Is a → **Workflow** (special-purpose).
- Reads → the **Telemetry** store.
- Writes → PRs via the repo provider against **Goober/Workflow/Gate** definitions.
- Gated by → a configurable human **Gate** (approval).

## Open questions

- **TUT-Q1:** ~~Detection method~~ **Resolved:** hybrid — metrics surface candidates,
  agentic step diagnoses & drafts (`TUT-003`).
- **TUT-Q2:** ~~Can the Tutor weaken/remove a required gate?~~ **Resolved:** yes — the
  Tutor authors freely; humans hold the quality bar through `config`-repo PR governance
  (branch protection / required review / CODEOWNERS), not via in-product restrictions
  (`TUT-006`, `TUT-009`).
- **TUT-Q3:** PR granularity — one PR per finding vs. consolidated training PRs.
- **TUT-Q4:** Preventing oscillation (repeatedly flipping a definition back and forth).
