# Design: Human-in-the-Loop — escalation visibility & intervention

> Status: **Draft for review — tiers 1–2 for build, tier 3 is a forward sketch** · Area prefix: `HITL` (new) · Milestone: **Human-in-the-Loop** (#16)
> Requirements: [`docs/requirements/gate.md`](../requirements/gate.md), [`docs/requirements/portal.md`](../requirements/portal.md)
> Architecture: [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md)
> Related issues: #168 (human-gate evaluator + durable pause/resume), #170 (CLI approve/approvals), #172 (access-control seam), #309 (surface terminal run_failed cause)

## 1. Why this exists

Runs reach a human for two reasons: a gate whose branch is **escalate-to-human**, or a run that hits a
**terminal state** (retries/repasses exhausted → `@escalate`/`@abort`/`failed`). Today both are just
"it's busted" — there is no *escalation experience*. This doc designs that experience in three tiers of
increasing power, brain-dump **item 13**.

Goal ladder (PO):

- **View escalations** — tier 1 (build now).
- **Minor fixes to unblock** — tier 2 (build now).
- **Extreme measures to unblock** — tier 3 (**sketch only**; v3/future, post-cloud).

## 2. Current state (grounded)

- **Human gates are not implemented.** `internal/gate/evaluate.go:118` returns
  `"human evaluator is not supported at V0 (GT-003, ships V1)"`. So #168 is unbuilt; the portal's
  `HumanGatePanel` + mock `GateApprovalRequest` are speculative UI ahead of the engine.
- **Automated/agentic escalation works:** bounded repass budget (`DefaultMaxRepasses = 3`) → on exceed,
  the gate branch is overridden to `@escalate` (`internal/gate/evaluate.go`), and `EscalationNotifier`
  posts a **comment on the driving issue/PR** (`internal/gate/escalate.go`) — that's the *entire* surface.
- Phases are `running/completed/failed/aborted/escalated` (`internal/journal/state.go`).
  `@escalate` → `PhaseEscalated`, retries-exhausted → `PhaseFailed`. The runner exposes a durable,
  human-triggered `ResumeFromTerminal` primitive for those two phases; the CLI/API action surface that
  invokes it remains separate work. Crash-`Resume` still only restarts interrupted running segments.
- The higher-level intervention actions remain unimplemented: no `goobers approve`/`approvals` (#170),
  no instruction addendum, no gate-verdict override, no checkout/drive, no access-control seam (#172).
  The failure *cause* is journaled as an `EventError` but there's no summarized "why it escalated"
  surface (#309).
- The journal **does** already carry what tier-1 needs: per-stage `Attempt`/`AttemptClass`, gate
  `repassAttempt`, artifact pointers per stage, phase, and timing.

## 3. Tier 1 — View escalations (build now)

Make escalated/terminal runs first-class and legible in **both CLI and portal** (the portal view is
milestone #14 DASH-6; this milestone owns the read model + CLI).

- **Escalation summary** (fixes #309): a durable, structured record on any run that reaches
  `@escalate`/`@abort`/`failed` capturing: the gate/condition that forced it, the branch/target chosen,
  repass/retry counts consumed, and the terminal cause message — not just the phase.
- **State-along-the-way inspection**: surface, per stage, the artifacts that existed at that point and the
  current state, using existing artifact pointers + journal events. CLI: extend `goobers status`/`trace`
  (or a new `goobers escalations`) to list escalated runs and drill into the cause + artifact timeline.
- View-only. No state change in tier 1.

## 4. Tier 2 — Minor fixes to unblock (build now)

Light-touch, **recorded**, one-off interventions that do not mutate the workflow definition. All of these
require the **durable pause/resume** engine capability (#168) and a **CLI action surface** (#170), routed
through the (future) access-control seam (#172). The dashboard calls these same actions (API-first, #14).

- **Rerun a stage with an instruction addendum**: re-execute a single stage of an escalated run with an
  explicit **one-off addendum** appended to the agent's instructions — e.g. an implement stage gets
  "fix it this way", or a reviewer stage gets "you must not block on X". The addendum is:
  - **not persisted** to the workflow definition (it's a one-off),
  - **recorded** in the run journal (who, when, what text, which stage/attempt) for auditability.
- **Force-pass / override a nondeterministic gate**: for agentic/human gates, a human can override the
  outcome to `pass` (or another branch), recorded as an explicit override event with rationale.
- **Resume semantics**: after a tier-2 action, the run leaves its terminal state and continues from the
  targeted stage/branch, re-pinned appropriately. This is the first case of *human-triggered* resume,
  distinct from crash-resume — the engine work in #168 must generalize resume to cover it.

**Recording is a first-class requirement**, not a nicety: every tier-2 action is an auditable journal
event so the play-by-play (and the Tutor, and telemetry) can see that a human intervened and how.

## 5. Tier 3 — Extreme measures (forward sketch only, v3/future)

> **Not scoped for build.** Captured here so the tier-1/2 designs don't foreclose it. No issues filed
> beyond a single tracking placeholder.

The most mature experience is to **"check out" a run** — effectively JIT-elevate into the run and drive
it manually: type code directly in the run's workspace, interact with the agent directly, step the
machine by hand. The act of JIT-elevation is itself recorded, and we record as much of the manual session
as possible (commands, edits, agent exchanges). This is security-sensitive (it's arbitrary
human-in-the-loop code execution inside a run's context) and depends on cloud/remote execution and the
access-control/identity work that lands post-V2. Design questions to answer *then*: elevation
authorization & audit, session capture fidelity, how a manually-driven run rejoins (or exits) the
deterministic machine, and blast-radius containment.

## 6. Dependency ordering

Tier 1 needs only the **read model + escalation-cause record** (#309-adjacent) — buildable immediately.
Tier 2 needs the **human-gate/pause-resume engine** (#168), the **CLI action surface** (#170), and rides
the **access-control seam** (#172). Both tiers are API-first so the portal (#14) consumes them without a
separate path. Tier 3 waits for cloud + identity (post-V2).

## 7. Issue breakdown (milestone #16)

- **[EPIC]** Human-in-the-Loop.
- HITL-1 (tier 1): Structured escalation/terminal-cause record (folds #309) — cause, gate/condition, counts.
- HITL-2 (tier 1): CLI escalation inspection — list escalated runs + drill into cause + per-stage artifact timeline.
- HITL-3 (tier 2): Human-gate + durable **human-triggered** pause/resume engine (generalizes resume; extends #168).
- HITL-4 (tier 2): CLI intervention surface — `goobers approve`/`override`/`rerun-stage` (extends #170).
- HITL-5 (tier 2): Rerun-stage **with recorded one-off instruction addendum** (not persisted to the workflow).
- HITL-6 (tier 2): Nondeterministic-gate force-pass/override, recorded with rationale.
- HITL-7: Access-control seam wiring for all tier-2 mutations (extends #172).
- HITL-8 (tier 3, **placeholder only**): "Check out / drive a run manually" — forward design, no build.

## 8. Open questions

- Where do addenda live in the envelope so they're passed to the agent but clearly marked one-off & audited?
- Override rationale: required field or optional? Leaning required for nondeterministic-gate overrides.
- Do tier-2 actions need the human-gate engine (#168) fully, or a lighter "resume-from-terminal" primitive
  first? Leaning: a `resume-from-terminal` primitive is the true dependency; the human-*gate* evaluator is
  adjacent but separable.
