# Spec: Gate

> Status: **Draft** · Derives from `../VISION.md` §6, §7 + early gate discussion · Area prefix: `GT`

## Purpose

A **Gate** is a validation state in a workflow that evaluates a condition and uses the
result to **branch the flow**. Gates are how the platform enforces quality, sign-off, and
human checkpoints.

## Model

**One Gate primitive, with a pluggable evaluator.** A gate is always "evaluate → branch";
what differs is the evaluator kind:

| Evaluator | What it does | Example |
|---|---|---|
| **Automated** | Runs a coded check over task outputs/conditions; deterministic outcome | tests pass, coverage ≥ X, task completed |
| **Agentic** | Invokes a scoped **reviewer goober** that returns a structured verdict | code review sign-off, design review |
| **Human** | Pauses the run for an explicit human decision | pre-merge approval, deploy approval, Tutor-PR approval |

This unifies automated checks, reviewer goobers, and the **configurable human gates** from
our earliest discussion under a single model. Which gates exist and whether they're
required is **configurable per workflow/instance**.

## Requirements

### Definition & branching
- **GT-001 (MUST):** A Gate MUST be defined as code as a state within a workflow.
- **GT-002 (MUST):** A Gate MUST produce an outcome that the workflow uses to branch
  conditionally; a failing/negative outcome MUST follow a defined branch (retry, route to
  fix, escalate, abort) — never a silent pass.
- **GT-003 (MUST):** A Gate MUST support all three evaluator kinds: automated, agentic,
  human.
- **GT-004 (SHOULD):** A Gate SHOULD support more than two branches (not just pass/fail).

### Evaluators
- **GT-010 (MUST):** An **automated** gate MUST run a coded check over task
  outputs/declared conditions and yield a deterministic outcome.
- **GT-011 (MUST):** An **agentic** gate MUST invoke a scoped reviewer goober and consume
  a structured verdict; it is invoked and telemetered like an agentic task
  (`GBO-012`/`GBO-013`/`GBO-020`).
- **GT-012 (MUST):** A **human** gate MUST pause the run and require an explicit human
  decision before proceeding, surfaced to the human (channel — see open questions).
- **GT-013 (SHOULD):** A human gate SHOULD support configurable timeout/escalation
  behavior (e.g. remind, auto-escalate, or auto-reject after N time).

### Configurability & telemetry
- **GT-014 (MUST):** Which gates are present and whether each is required MUST be
  configurable per workflow/instance.
- **GT-015 (MUST):** Every gate evaluation MUST record its outcome (and rationale, where
  available) to the goober-run telemetry store — the Tutor relies on gate results.

## Relationships

- Part of → a **Workflow** (a state in the machine).
- Agentic gates invoke → a reviewer **Goober**.
- Human gates surface to → the **Portal** / notification channel (scope TBD).
- Emits → gate-result telemetry consumed by the **Tutor**.

## Open questions

- **GT-Q1:** Structured **verdict schema** for agentic reviewer gates (pass/fail +
  findings + severity?).
- **GT-Q2:** **Delivery channel for human gates.** This tensions with "portal is
  observability, config is code, no UI." A human approval needs *some* interactive
  surface (portal action? PR review? chat/notification?). Resolve in the Portal spec.
- **GT-Q3:** Branch expression syntax for multi-branch gates.
- **GT-Q4:** Can a single gate combine evaluators (e.g. automated check AND human
  approval), or is one evaluator per gate (chain gates instead)?
