# Spec: Gate

> Status: **Draft** · Aligned to ../ARCHITECTURE.md (2026-07-12) · Derives from ../VISION.md §6, §7 · Area prefix: GT

## Purpose

A **Gate** is a validation state in a workflow that evaluates a condition and uses the
result to **branch the flow**. Gates are how the platform enforces quality, sign-off, and
human checkpoints — with the same taxonomy at every deployment tier.

## Model

**One Gate primitive, with a pluggable evaluator.** A gate is always "evaluate → branch";
what differs is the evaluator kind:

| Evaluator | What it does | Example |
|---|---|---|
| **Automated** | Runs a coded check over task outputs/conditions; deterministic outcome | tests pass, coverage ≥ X, poll remote CI and branch on result |
| **Agentic** | Invokes a scoped **reviewer goober** that returns a structured verdict | code review sign-off, design review |
| **Human** | Pauses the run for an explicit human decision | pre-merge approval, deploy approval, Tutor-PR approval |

This unifies automated checks, reviewer goobers, and **configurable human gates** under
a single model. Which gates exist and whether they're required is **configurable per
workflow/instance**. A gate is a **machine state, not a stage**: its automated and
agentic evaluators run with stage-execution semantics, but the gate itself carries no
stage contract (`ARCHITECTURE.md §5`).

The taxonomy is tier-independent; what changes by tier is **how long a gate can wait**.
At tiers 1–2 human gates are approvals resolved via the CLI or portal while the local
runner daemon holds the run (or parked and resumed per `GT-022`). At tier 3 the
Temporal runner adds **durable multi-day waits** — a human gate can pause a run across
days without holding a process open (`ARCHITECTURE.md §3.2`). Gate verdicts are
journal events either way.

## Requirements

### Definition & branching
- **GT-001 (MUST):** A Gate MUST be defined as code as a state within a workflow.
  *(All tiers)*
- **GT-002 (MUST):** A Gate MUST produce an outcome that the workflow uses to branch
  conditionally; a failing/negative outcome MUST follow a defined branch (retry, route to
  fix, escalate, abort) — never a silent pass.
- **GT-003 (MUST):** A Gate MUST support all three evaluator kinds: automated, agentic,
  human. *(All tiers; the human evaluator's implementation lands V1 — automated and
  agentic ship at V0.)*
- **GT-016 (MUST):** A Gate MUST have **exactly one** evaluator. Combined conditions are
  expressed by **chaining** gates in sequence (e.g. automated check → human approval),
  not by bundling evaluators into one gate.
- **GT-004 (SHOULD):** A Gate SHOULD support more than two branches (not just pass/fail).

### Evaluators
- **GT-010 (MUST):** An **automated** gate MUST run a coded check over task
  outputs/declared conditions and yield a deterministic outcome. Its evaluator runs
  **with stage-execution semantics** — declared env, timeout, retry policy, in the
  stage working copy — while the gate itself remains a machine state, not a stage
  (`ARCHITECTURE.md §5`).

  The built-in predicates return `pass` when true and `fail` when false:

  | Check | Required params | Predicate |
  |---|---|---|
  | `status-equals` | `equals` optional (defaults to `success`) | stage status equals the configured value |
  | `output-equals` | `key`, `equals` | flattened output string equals the configured value |
  | `output-not-equals` | `key`, `equals` | flattened output string differs from the configured value |
  | `output-numeric-gte` | `key`, numeric `threshold` | numeric output is `>= threshold` |
  | `output-numeric-lte` | `key`, numeric `threshold` | numeric output is `<= threshold` |
  | `output-numeric-lt` | `key`, numeric `threshold` | numeric output is `< threshold` |
  | `output-matches` | `key`, valid RE2 `pattern` | flattened output string matches the pattern |
  | `ci-status` | `equals` optional (defaults to `passing`) | CI status equals the configured value |

  Numeric thresholds and regular expressions are validated while the workflow is
  compiled, so invalid gate configuration is rejected before a run starts. The
  `ci-status` check additionally exposes `timeout`; the merge-specific
  `land-outcome` and `queue-outcome` checks expose their documented multi-branch
  outcomes rather than boolean predicates.
- **GT-011 (MUST):** An **agentic** gate MUST invoke a scoped reviewer goober and consume
  a structured verdict; it is invoked and telemetered like an agentic task
  (`GBO-012`/`GBO-013`/`GBO-020`) — invocation/result envelopes and artifact pointers,
  same as any stage.
- **GT-012 (MUST):** A **human** gate MUST pause the run and require an explicit human
  decision before proceeding, surfaced to the human. *(Tiers 1–2)*: a short-lived
  approval resolved via the CLI or portal while the daemon holds the run.
  **Tier 3 (V2):** the Temporal runner makes the same gate a durable wait that can
  span days (see `GT-021`).
- **GT-013 (SHOULD):** A human gate SHOULD support configurable timeout/escalation
  behavior (e.g. remind, auto-escalate, or auto-reject after N time).

### Configurability & telemetry
- **GT-014 (MUST):** Which gates are present and whether each is required MUST be
  configurable per workflow/instance.
- **GT-015 (MUST):** Every gate evaluation MUST record its outcome (and rationale, where
  available) as run-journal events, projected into the goober-run telemetry store —
  the Tutor relies on gate results. *(All tiers)*

### Tier behavior & CI polling
- **GT-020 (MUST):** An automated gate MUST be able to **poll a remote CI system**
  for a referenced check/run and branch on its result — the primitive behind the V0
  implementation workflow's CI-poll/repass loop (bounce failures back to the
  implementer stage). Polling cadence/timeout are declared on the gate. *(All tiers)*
- **GT-021 (MUST):** **Tier 3 (V2):** human gates MUST support **durable multi-day
  waits** via the Temporal runner — the run persists across process and worker
  restarts while awaiting a decision, with no daemon process held open. This is the
  cloud drop-in for the gate-wait seam; the gate definition is unchanged from tiers
  1–2 (`ARCHITECTURE.md §3.2`).
- **GT-022 (MUST):** *(Tiers 1–2)*: a human gate pending past a configurable
  bound MUST park the run safely (checkpointed in the journal) rather than fail it,
  so an approval arriving after a daemon restart still resumes the run — a pending
  approval is never lost to a restart.

## Relationships

- Part of → a **Workflow** (a state in the machine).
- Evaluated by → the **Runner**, with stage-execution semantics for automated/agentic
  evaluators (local at tiers 1–2; Temporal at tier 3).
- Agentic gates invoke → a reviewer **Goober**.
- Human gates surface to → the **CLI/Portal** / notification channel.
- Automated gates may poll → remote **CI** (`GT-020`).
- Emits → gate-result journal events consumed by the **Tutor**.

## Open questions

- **GT-Q1:** ~~Verdict schema for agentic reviewer gates~~ **Resolved (shape):** mirrors
  the task result envelope — `decision (pass|fail|needs-changes), findings:[{severity,
  message, location}], summary`. *(Remaining: finalize fields.)*
- **GT-Q2:** **Resolved (updated):** human gates are approved via **CLI or portal** at
  tiers 1–2 (`PORT-011`), with notifications linking back; code-merge gates may also
  ride the git PR. At tier 3 the same approval surfaces resolve a durable Temporal
  wait (`GT-021`, `ARCHITECTURE.md §3.2`).
- **GT-Q3:** *(build-time design)* Branch expression syntax for multi-branch gates.
- **GT-Q4:** ~~Combine evaluators per gate?~~ **Resolved:** no — one evaluator per gate;
  chain to compose (`GT-016`).
