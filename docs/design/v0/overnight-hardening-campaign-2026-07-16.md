# Overnight hardening campaign — 2026-07-16

Design record for the first long-running autonomous self-improvement test. This
doc is the map for the ~35 items approved for the queue on 2026-07-16: the
invariant families they enforce, the dependency edges between them, and the
coordination hazards sibling PRs need to respect. It is written for three
audiences: the implementation/merge-review/pr-remediation workflows executing
the queue, a human returning mid-run, and future triage passes hunting the same
bug shapes.

Scope steering for this campaign (operator direction): quality, resilience,
QoL, polish, DSL flexibility, and general maturity — hardening what exists, not
new product surface. Level-2 ("large teams, monorepo, cloud")-only items were
deliberately excluded.

## 1. The invariant families

Nearly everything approved tonight enforces one of five invariants. New code —
and reviews of tonight's PRs — should check against them explicitly.

### I1 — Terminal cleanup is a lifecycle guarantee, not a happy-path side effect

Established by #493/#498 (claims) and repeatedly re-found elsewhere: any
resource acquired by a run (claim-ledger lease, worktree, remote run branch,
concurrency slot) must be released by reaching *any* terminal phase, through
one path (`FinalizeTerminal`), never by an individual stage's success flow.

- #520 — WF-016 resume refusal must set a canonical terminal phase (`failed`);
  prerequisite for #498's guarantee to reach that path.
- #536 — worktree teardown on abort/fail/escalate + orphan sweep (144M leak
  observed live).
- #553 — remote run-branch deletion on terminal-without-PR and after merge.
- #544 — `ResultBlocked` fails closed at V0 instead of producing an immortal
  non-terminal run.
- #535 — the concurrency slot itself: reconcile and resume must agree on
  terminality (see I2).

### I2 — The event log is the only source of truth for terminality

`internal/journal/reader.go:61-67` states it; #535 enforces it where it was
violated (`ActiveRunCounts` trusting `state.json`, leaking slots on every
restart after an fsync-window crash). Anything deciding "is this run over?" —
retention (#550/#551), watchdog (#547), status surfaces — uses `rd.Phase()`.

### I3 — No silent failure of the machinery that keeps the machinery alive

- #533 — scheduler journal duplicate `seq` breaks 100% of scheduler-event
  telemetry ingest (live now); fix emission AND make ingest idempotent.
- #554 — periodic claim-recovery/delegation sweeps currently swallow all
  errors; route to the instance journal.
- #541 — gate branches resolving to `@escalate`/`@abort` must fire the
  escalation notifier (today: silence on the driving issue).
- #546/#547 — heartbeat during long agentic attempts, then a stalled-run
  watchdog on top of it (in that order).

### I4 — An issue's labels must always reflect what the pipeline may do with it

- #539 — reviewer-fail/`@abort` parks the issue (`goobers:needs-human`),
  breaking the FIFO re-claim starvation loop. The highest-leverage single item
  in the queue for autonomous throughput.
- #534 — `backlog-query` comma-split fix; the shipped curation idempotency
  exclusion is currently a no-op.
- #481 + #531 — claim-ledger exclusivity for PR selection (pr-select and
  gather-pr-context), sharing one `pr/<number>` key namespace.

### I5 — Deterministic values outrank model-echoed values

Completion-contract ruling (#297 family) extended to review plumbing: #540
makes `apply-verdict` pin on the deterministic `selectedHeadSha`, treating the
reviewer's echoed SHAs as a cross-check. Same spirit: #562 (stage-qualified
`inputsFrom`) removes the echo-hop pattern that produced the live post-merge
failures (#413/#479 class).

## 2. Dependency / sequencing edges

Most items are independent single-PR units. The edges that matter:

| First | Then | Why |
|---|---|---|
| #546 (heartbeat) | #547 (watchdog) | Watchdog without heartbeat false-kills healthy 20-min implements |
| #550 (prune core + CLI) | #551 (daemon auto-retention) | Same prune core, tested once |
| #520 (WF-016 terminal phase) | — | #498 already landed; whichever consumer touches that path adds the cross-test |
| #481 or #531 (either order) | the other | Shared claim-key namespace; second lands the cross-entrypoint test |
| #487/PR #516 (trace prefix) | #558 (abort/redact prefix) | Shared `resolveRunID` helper |
| #539 (park-on-fail) | #541 (branch notify) | Both comment on terminal; second must not double-post |
| #543 (exit codes) | #559 (run progress/follow) | Both restructure `waitForRunTerminal`; trivially conflicting edits |
| #549 (dispatch noise) ~ #551 (retention) | — | Independent, but both delete run-dir shapes; later one rebases |

## 3. Sibling-PR collision hazards (for merge-review)

The known failure mode (#377/#378, #353): two green PRs touching the same
package can break main on the second merge. Tonight's queue has these likely
file-cluster overlaps — treat as co-review clusters, expect serial rebases:

- `cmd/goobers/run.go`: #543, #559, #558.
- `cmd/goobers/up.go` / `daemon.go`: #547, #554, #555, #537, #535.
- `internal/worktree/`: #529, #536, #548.
- `internal/runner/run.go` (taskOutcome/gate switch): #541, #544, #561, #562.
- `internal/workflow/` + `api/validate`: #560, #563, #564, #565.
- `selfhost/.../merge-review.yaml`: #540, #562, #568.
- Claim ledger (`internal/localscheduler/claim.go`): #481, #531, #494.
- `cmd/goobers/status.go`/`runs.go`: #556, #557.

The shared gate-evaluator warning stands: anything touching
`internal/gate.Evaluate` (#263, #541) hits every agentic gate — audit
`grep -rl "evaluator: agentic"` before landing.

## 4. Design questions deliberately left open (captured, not approved)

- #482 — where claim/in-progress state canonically lives (remote store vs
  ledger) and cross-run visibility.
- #507 — who owns test-suite quality (tutor vs work-nomination vs a new
  canonical workflow); #533/#506 make the underlying signals exist first.
- #522 — general `goobers doctor`/reconcile pass; tonight's #535/#536/#537
  fixes shrink its scope — re-scope it after they land.
- #509 — selection priority beyond FIFO; interacts with #519's fairness layer
  (which workflow gets a slot) and stays a separate layer (which item it picks).
- #491 — parameterized manual-run args; #564's webhook trigger deliberately
  excludes payload→args until this is designed.

## 5. Rulings recorded on issues (quick index)

Maintainer rulings unblocking previously human-gated items, with the decision
inline on the issue: #489 (manual trigger kind, approach b), #492
(`github:issues:approve` capability at admission), #494 (force-release
semantics + delegation contract), #499 (5m drain default + `--drain-timeout` +
official double-Ctrl-C), #506 (ci-checks.json artifact contract + one scalar),
#520 (`failed` + WF-016 grepability), #519 (starvation-aged round-robin
dispatch), #523 (verdict-level digest cache).
