# Design: Human-in-the-Loop — escalation visibility & intervention

> Status: **draft — for review; tiers 1–2 for build, tier 3 is a forward sketch** · Area prefix: `HITL` (new) · Milestone: **Human-in-the-Loop** (#16)
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

- **Human gates are implemented at the runner seam.** Reaching one durably records
  `gate.paused`; `Runner.Resume` accepts an explicit gate-scoped decision and records the
  selected configured branch as `gate.evaluated`. Decisions bind to the paused event's sequence
  so a delayed request cannot resolve a later visit to the same gate. A restart with no decision
  remains paused, and an unknown, mismatched, stale, or unauthorized decision fails closed.
  Configured approvers are enforced against the submitted actor identity. `goobers approve` and the
  versioned intervention API submit those decisions through the shared access-control seam;
  configured timeout behavior is rejected until the runner can enforce it durably.
- **Automated/agentic escalation works:** bounded repass budget (`DefaultMaxRepasses = 3`) → on exceed,
  the gate branch is overridden to `@escalate` (`internal/gate/evaluate.go`), and `EscalationNotifier`
  posts a **comment on the driving issue/PR** (`internal/gate/escalate.go`) — that's the *entire* surface.
- Phases are `running/completed/failed/aborted/escalated` (`internal/journal/state.go`).
  `@escalate` → `PhaseEscalated`, retries-exhausted → `PhaseFailed`. The runner exposes a durable,
  human-triggered `ResumeFromTerminal` primitive for those two phases; the CLI/API approve and
  override actions invoke it. Crash-`Resume` restarts interrupted running segments and preserves
  unresolved human-gate pauses without advancing them.
- Tier-2 intervention actions are available through the API-first `goobers approve`,
  `goobers override`, and `goobers rerun-stage` commands. The tier-3 checkout/drive surface remains
  unimplemented.
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

Light-touch, **recorded**, one-off interventions that do not mutate the workflow definition. The durable
pause/resume engine capability (#168), CLI action surface (#170), and access-control seam (#172) are
available. The dashboard calls these same versioned API actions (API-first, #14).

- **Rerun a stage with an instruction addendum**: re-execute a single stage of an escalated run with an
  explicit **one-off addendum** appended to the agent's instructions — e.g. an implement stage gets
  "fix it this way", or a reviewer stage gets "you must not block on X". The addendum is:
  - **not persisted** to the workflow definition (it's a one-off),
  - **recorded** in the run journal (who, when, what text, which stage/attempt) for auditability.
- **Force-pass / override a nondeterministic gate**: for agentic/human gates, a human can override the
  outcome to `pass` (or another branch), recorded as an explicit override event with rationale.
- **Resume semantics**: after a tier-2 action, the run leaves its terminal state and continues from the
  targeted stage/branch, re-pinned appropriately. This is the first case of *human-triggered* resume,
  distinct from crash-resume; the runner now provides both explicit human-gate decisions and
  `ResumeFromTerminal`.

**Recording is a first-class requirement**, not a nicety: every tier-2 action is an auditable journal
event so the play-by-play (and the Tutor, and telemetry) can see that a human intervened and how.
Terminal actions use normative `run.resumed` action, actor, gate, decision, rationale, and target
fields; its `complete` field is also the durable crash-recovery marker when the selected branch is
`@complete`.

## 4a. Tier 2 on the engine — the `goobers.hitl.v1` protocol (#3883, decision 005 R8)

Everything in §4 is described at the **runner** seam: `Runner.Resume`,
`Runner.ResumeFromTerminal`, `Runner.RerunStage`. Those are in-process calls that open the run's
journal and drive its state machine directly. For a run the **tier-3 engine** drives on Temporal,
all three are wrong — the daemon's runner has never executed a stage of it, and its journal has a
live writer on the far side of a workflow — so since #3847 every intervention on an engine-driven
run was refused outright. That refusal is what blocked the `merge-review` (M3) and `pr-remediation`
(M4) lane cutovers.

`goobers.hitl.v1` is the second destination for the *same* operator verbs.

### Protocol identity and version

| | |
|---|---|
| Protocol | `goobers.hitl.v1` (`engine.HITLProtocol`) |
| Version | `1` (`engine.HITLProtocolVersion`) |
| Transport | Temporal **workflow Update** named `goobers.hitl-intent.v1` (`engine.HITLUpdateName`) |
| Introspection | Temporal **Query** named `goobers.hitl-state.v1` (`engine.HITLStateQuery`) |

The version appears in **three** places, and each carries a different obligation:

- **In the update name.** A future `goobers.hitl-intent.v2` is registered *alongside* v1, so a
  rolling worker-fleet upgrade never has a window in which an in-flight run speaks a protocol its
  worker does not.
- **In the payload** (`protocol` + `version`). Mismatch is refused with
  `hitl_protocol_unsupported` before any history is written, so a daemon rolled forward past its
  workers gets a clean, named refusal rather than a wedged run.
- **In the pinned run input** (`RunInput.HITL`). The posture is decided at run start and never
  re-read, for the same WF-016 reason every other run control is pinned.

### Why an Update, not a Signal

A signal is fire-and-forget. The client learns that the *server* accepted the signal, never that
the *workflow* accepted the intent — and every one of these intents can be legitimately refused
(the gate has no such branch, the gate is deterministic, the run is still executing, the terminal
the operator quoted has moved). Acknowledging an operator's "approve" and then silently dropping
it is exactly the failure this design cannot have.

A workflow Update carries a return value and an error back to the caller, and both the acceptance
and the outcome are durably in history before `handle.Get` returns. The daemon submits with
`WaitForStage: WorkflowUpdateStageCompleted`, so **the API never reports success before durable
workflow acceptance**. The in-tree precedent is `internal/engine/schedule.go`'s reconcile update.

### The three intents

| Intent | Runner analogue | Resumes at |
|---|---|---|
| `resolve-escalation` (`approve` / `override` / `deny`) | `ResumeFromTerminal` via `approve`/`override`, plus the deny marker | the gate's branch target for the operator's decision |
| `rerun-stage` | `RerunStage` | the named stage, re-dispatched with the operator's addendum |
| `resume-from-terminal` | `ResumeFromTerminal` (raw form) | an explicit target state, or `@complete` |

`approve` and `override` name a gate; `deny` does not, because it resumes nothing. `override` and
`deny` require a rationale. `rerun-stage` requires an instruction addendum and is accepted only for
an **escalated** run whose stage is agentic under an agentic gate — `Runner.RerunStage`'s
`validateRerunTarget` rule, verbatim.

### Phase matrix — reject, never queue

The workflow is in exactly one of three phases, reported by the state query:

| Phase | Meaning | An intent arriving now |
|---|---|---|
| `executing` | the walk is running a stage or gate | refused `hitl_run_executing` |
| `awaiting-operator` | a resumable terminal is journaled and held open | **accepted** |
| `settled` | the run is closed | refused `hitl_run_settled` |

Intents are **refused, not buffered**. Queueing an intent would mean holding an operator's verdict
against a gate that has not been evaluated yet, a stage rerun for an attempt still in flight, or a
resume for a terminal the run has not reached — three ways to apply a decision to a state the
operator never saw. The refusal names the phase, so the daemon tells the operator to re-read the
run and reissue rather than retry blindly.

The one deliberate exception is an intent whose **request id this run has already answered**: it is
admitted in any phase so its recorded answer can be replayed idempotently.

### Ordering, idempotency, and the compare-and-set guard

- **Request id.** Every intent carries one. The daemon uses the HTTP `Idempotency-Key` verbatim
  when present, and the request's own payload fingerprint when not — so an identical retry
  deduplicates either way, and a genuinely different decision cannot collide with the first.
- **Three dedup layers.** The request id *is* the Temporal `UpdateID`, so the **server**
  deduplicates a retried delivery. The workflow pins the payload fingerprint per request id, so a
  key reused for a **different** payload is refused `hitl_request_id_reused`. And the workflow
  replays the first delivery's ack (with `duplicate: true`) for an exact repeat.
- **Terminal generation.** `expectedTerminalGeneration` is the engine's analogue of the runner's
  `ExpectedTerminalSeq`. It is the number of terminals the run has produced — identical to the
  count of `run.finished` events in the journal, which is how the daemon computes it and how an
  operator can verify it. A mismatch is refused `hitl_terminal_generation_changed`. This is what
  makes out-of-order and stale intents safe: an intent issued against generation *N* cannot land on
  generation *N+1*.
- **Concurrency.** Handlers serialize on a workflow mutex and re-check phase *and* generation under
  the lock, because the validator ran against a pre-lock snapshot. Two operators racing on one
  escalated run produce exactly one resumption; the loser is refused by name.

### Identity, authorization, and audit

The operator's identity travels **in the intent** and is enforced **inside the workflow**: when the
pinned policy names an actor set, an intent from anyone else is refused `hitl_unauthorized`. The
daemon's own access-control seam still applies first; the workflow check is the second, so a
compromised or misconfigured daemon cannot resolve a run it was never entitled to resolve.

Run containment is enforced the same way: an intent naming a different `runId` than the workflow's
pinned one is refused `hitl_run_mismatch`, so an intent can never be delivered to the wrong run
even if it is addressed to the wrong workflow.

Audit is journal parity, not a parallel record. An accepted intent writes the **same events the
runner writes**:

- `rerun-stage` → `stage.rerun.requested` with `stage`, `attempt` (cumulative), `attemptClass:
  human`, `actor`, and `instructionAddendum`.
- `resolve-escalation` / `resume-from-terminal` → `run.resumed` with `status`, `target`,
  `complete`, `actor`, `action`, `gate`, `decision`, and `rationale`.
- `deny` → a `runner.annotation` whose `kind` is `escalation.resolution` — deliberately the same
  string the in-process deny path uses, so one grep finds both drivers.

Each also carries a `runner` provenance map naming the protocol, its version, the intent kind, the
idempotency key, and the actor.

### Terminal-hook and claim behaviour

The terminal is **journaled before the hold, not after it**. A run that escalates writes
`run.failed`/`run.finished` and only then parks. So the journal, the DS5 projection, the read model
and the portal all see an escalated run at exactly the moment they saw one before; the generation
an operator quotes is on disk before they can quote it; and an expired hold leaves that terminal
standing with nothing further written.

What *does* change while the hold is open is that the run's **Temporal workflow stays open**, and
therefore its scheduler concurrency slot stays occupied. That is why the protocol is opt-in and why
the window is configurable — see rollback below.

### Refusal codes → HTTP

| Code | HTTP | Intervention code |
|---|---|---|
| `hitl_unauthorized` | 403 | `intervention_forbidden` |
| `hitl_invalid_intent` | 400 | `invalid_intervention` |
| `hitl_protocol_unsupported` | 400 | `protocol_unsupported` |
| `hitl_request_id_reused` | 409 | `idempotency_key_reused` |
| `hitl_terminal_generation_changed` | 409 | `terminal_generation_changed` |
| `hitl_run_executing` / `hitl_run_settled` | 409 | `run_not_intervenable` |
| `hitl_not_resumable` | 409 | `gate_not_approvable` |
| `hitl_run_mismatch` | 409 | `run_identity_mismatch` |
| `hitl_not_enabled` | 409 | `run_engine_driven` |

An error that is **not** one of these codes is reported as a 500: a transport failure must never be
laundered into "the run refused you".

### Replay determinism

Three history shapes are proved against a real `worker.WorkflowReplayer` over a real dev server
(`internal/engine/hitlreplay_test.go`):

1. a history containing an **accepted update** — the hold timer, the update acceptance and
   completion, the resumption, and the stages after it;
2. a **pre-protocol** history (`RunInput.HITL` unset) — the invariance case, which is every run
   recorded before #3883;
3. a history in which the **hold expired** unanswered, which is what an unattended production
   escalation records.

Invariance is structural, not incidental: with no policy pinned, the terminal hook is a pure no-op,
so no timer is created and no extra journal write is issued. `RunInput.HITL` is `omitempty`, so an
instance that did not opt in serializes byte-identically to one that predates the protocol.

### Configuration and rollback

```yaml
engine:
  hostPort: 127.0.0.1:7233
  namespace: default
  taskQueue: goobers
  hitl:
    enabled: true        # default false
    window: 4h           # default 24h when enabled
    actors: [ops@example.com]   # optional; empty means any authorized actor
```

**Rollback is total and requires no code change.** Setting `enabled: false` (or removing the
`hitl:` block) means new runs pin **no** policy, and a run with no policy settles at its terminal
exactly as it did before #3883. The update handler stays registered — so an intent addressed to
such a run is refused `hitl_not_enabled` rather than timing out — and in-flight runs started under
the old config are unaffected, because the policy is pinned at start. Detaching the daemon's engine
client (or running a type-1/type-2 daemon) restores the #3847 `run_engine_driven` refusal verbatim.

### Named, bounded drift from the runner

The engine numbers a stage's **dispatch** attempts per-dispatch starting at 1
(`dispatchWithRetry`), where the runner numbers them cumulatively across the run. #3883 does not
change that. The parity-visible record — the `stage.rerun.requested` event's cumulative `attempt`
and its `attemptClass: human` — matches the runner exactly; the per-dispatch attempt number inside
a re-dispatched stage does not. Threading a cumulative attempt base through the dispatch path is
tracked separately and is deliberately out of R8's scope.

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
