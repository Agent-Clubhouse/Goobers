# Repass budgets bound a gate, not a run: add a per-run ceiling, re-size the default from observed convergence, and stop projecting `repass_count` as zero

Suggested labels: `area:runner`, `area:contracts`, `area:telemetry`, `type:feature`, `goobers:needs-human`

## Problem

An operator who writes `maxRepasses: 3` reads it as "this stage will be re-run at
most three times." What is enforced is the consecutive non-pass count at a single
gate, reset to zero every time that gate passes. In any workflow where two gates
can route back to the same agentic stage — which is the shape of the shipped
implementation workflow — neither counter ever accumulates, and the declared
budget is never the thing that stops the loop. One run executed 16 implement
sessions across 4h45m and terminated on the identical-diff guard, not on its
budget.

The operator cannot see this happening. The read model's `repass_count` is zero
for every run ever recorded, because the projector counts only operator-requested
reruns. A month in which 71% of model spend went to runs that never completed
looks, in the product, exactly like a month in which none of it did.

There is a second, quieter consequence waiting. Correcting the accounting without
re-sizing the number changes what the number costs: at the shipped default of 3,
per-stage accounting would have truncated roughly one in seven of the runs that
actually succeeded. The budget was never calibrated, because until now it never
bound anything.

## Evidence

- `docs/audits/2026-08-08-gaggle-reliability/domains/audit-goobers-runs.md`,
  finding `repass-budget-not-composed`: run `9824e10c4250fd9fcf533d45b0431101`
  (escalated 2026-08-03, 4.75h) journals 16 `implement` `stage.finished` success
  events; `gate.evaluated` shows `review` needs-changes ×10 / pass ×6 and
  `local-gate` fail ×5; the terminal event carries `runner.repassAttempt=4`.
  326 implementation runs since Aug 1 had more than one traversal.
- Same file, finding `readmodel-repass-count-dead`: `SELECT MAX(repass_count)
  FROM run` returns 0 across all gaggles and all time, against telemetry showing
  traversals up to 17.
- `docs/audits/2026-08-08-gaggle-reliability/README.md`, "The healthy gaggle's
  hidden layers" item 4: $1,100.34 of $1,551.45 August model spend on
  failed/escalated/aborted runs.
- `docs/audits/2026-08-08-gaggle-reliability/coldstart/coldstart-dotnet.md`
  finding 3: `DefaultMaxRepasses = 3` is documented only in
  `docs/design/human-in-the-loop.md`, and whether a parallel's `onFailure` route
  counts against a repass budget is stated nowhere — a first-week author cannot
  tell whether their implement → dual-review → implement loop is bounded at all.
- Upstream code: `internal/runcontrol/runcontrol.go:13` (`DefaultMaxRepasses = 3`);
  `api/v1alpha1/workflow_types.go:311-325` (`RunControls` = `MaxRepasses`,
  `StalledRunTimeout`, `MaxRunDuration`); `internal/gate/evaluate.go:131`
  ("MaxRepasses is the inherited run budget" — the comment says run, the
  enforcement is per gate) and `internal/gate/evaluate.go:462-468` (reset to 0 on
  pass, otherwise increment the per-gate key).
- Projection feasibility: `internal/gate/journal.go:23-30` already writes
  `Runner["repassAttempt"]` onto every `gate.started`/`gate.evaluated` event, and
  `internal/readservice/runs.go:2039` already reads it; the projector
  (`internal/readmodel/project.go:348-352`) increments `RepassCount` only under
  `journal.EventStageRerunRequested`, documented at `internal/journal/event.go:31-33`
  as an operator-requested rerun.
- Convergence distribution, `telemetry.db`, gaggle `goobers`, `workflow LIKE
  'implementation%'`, `started_at >= '2026-08-01'`, the 288 runs that reached the
  `implement` stage (108 of which completed):

  | `implement` entries | runs | completed | completions forfeited by a cap here |
  |---|---|---|---|
  | 1 | 79 | 40 | — |
  | 2 | 61 | 24 | — |
  | 3 | 39 | 16 | — |
  | 4 | 54 | 12 | 16 (14.8%) |
  | 5 | 20 | 3 | 13 (12.0%) |
  | 6 | 13 | 4 | 9 (8.3%) |
  | 7 | 9 | 6 | 3 (2.8%) |
  | 8 | 6 | 3 | 0 |
  | 9–17 | 7 | 0 | 0 |

  Marginal model spend beyond a cap, same population (`stage_model_usage` joined
  on `traversal > N`), against $1,150.23 total implementation spend: >4 entries
  $96.82, >5 $62.50, >6 $42.87, >8 $18.77. Every run needing a 9th entry (7 runs,
  $18.77) failed. Runs needing 5–8 entries produced 16 completions for $96.82 of
  marginal spend — about 1.5× an average run's cost per completion, which is
  expensive but not waste.

## Proposed direction

Three changes, one design. The first is a dependency, not this issue's work; the
other two are.

**1. Take per-stage accounting as given.** The accounting defect — reset on pass,
counters keyed by gate — is already filed, approved, and unimplemented (see
Duplicate search). This proposal assumes it lands and does not re-specify it.

**2. Add a per-run ceiling that never resets.** A new `runControls` field,
`maxRunRepasses`, bounding the total number of gate-driven returns to an earlier
state within one run, across every gate and every target stage. It inherits
instance → gaggle → workflow exactly like its siblings, and it is seeded on
resume from the journal the same way the per-gate counters are, so a restarted
run continues its ceiling rather than buying a fresh one. When it trips, the run
escalates through the existing terminal path with a cause that names the run
ceiling, so the escalation says what actually happened instead of blaming the
identical-diff detector.

Per-stage accounting alone does not give this. A workflow with two agentic
stages that each loop — an implementer and a CI remediator, the shipped shape —
is bounded per stage and unbounded per run; the ceiling is the only field that
answers "how much will one run cost me, worst case."

**3. Size the defaults from the distribution above, and say so out loud.**
This is the part that most needs review, because it is a throughput decision
disguised as a constant:

- Raise the default per-stage budget from 3 to **6 repasses** (7 entries). At 3,
  correcting the accounting silently forfeits 16 of 108 observed completions. At
  6 it forfeits 3, for $42.87 of avoided marginal spend in the observed month.
- Set the default run ceiling at **8 repasses**. Observed conversion past the 8th
  entry into a stage is zero across 7 runs; this is the one cut that costs
  nothing.
- Land the raise **with** the accounting fix, never before it. Ahead of that fix
  a raise is pure loosening of a bound that already doesn't bind.

**Zero-config behavior.** A user who writes no `runControls` block gets 6 per
stage and 8 per run, and `maxRunDuration` stays unset as today. Loops terminate,
generously — high defaults with the DSL as the place you tighten, not the place
you have to go to get correctness. Nothing about a normal workflow requires the
author to know the field exists; a lane that wants a tighter budget writes one
line.

**4. Project gate repasses into `repass_count`, and split reruns out.**
`repass_count` should count what its name says: gate-driven repasses, derived
from `gate.evaluated` events the runner already writes. Operator-requested
reruns move to their own `rerun_count`, because conflating them destroys the
signal the outcome-summary and no-work-filter work will need. Both project from
existing events, so history backfills by reprojection rather than new
instrumentation. Surface it: a repass column on the run list and a per-workflow
convergence histogram — an operator should be able to see the table in this
issue without writing SQL against telemetry.

**5. Relationship to the single remediation-budget primitive (VISION wish 4).**
Different axes; do not merge them. That primitive is cause-scoped — one
declaration bounding a remediation loop per cause, covering causes the runtime
grows later, replacing four hand-listed inputs that hard-fail the stage when one
is omitted. The run ceiling is cause-blind and run-scoped. The useful composition
is that the ceiling is the backstop when no per-cause budget applies: a cause the
workflow never declared should inherit a default bounded by the run ceiling
rather than crashing the stage, which is exactly the live failure that wish was
written from. The cause-scoped half is specified in the companion draft
`one-budget-primitive.md`; this issue needs only the run ceiling to exist and to
be inheritable, and the two should be reviewed together so the units and the
inheritance chain match.

**6. Version placement.** `runControls` is gaggle/workflow spec surface. The
DSL 2.0 epic's investigation establishes that the 2.0 interpreter is a strict
superset of 1.4 and that 1.4 is on its way to deprecation, so `maxRunRepasses`
should be added to 2.0 only and reached via the existing `fix --to 2.0`
migration, rather than growing 1.4 on the way out.

## Alternatives considered

- **The shipped total-elapsed-time run timeout.** Wall clock, not work. A 4h45m
  run under a 4h cap dies mid-session and discards committed work, while the same
  16 sessions on a faster host finish under the cap. Keep it as the outermost
  net; it does not answer how many times a stage may re-run.
- **A per-run cost ceiling from the proposed token/cost budget hierarchy.** Right
  axis for spend, wrong instrument for a loop: it cannot tell one expensive
  productive session from eight futile ones, and it terminates mid-stage. Both
  should exist — the loop bound is a count, the spend bound is a currency.
- **Leave the identical-diff guard as the de-facto bound.** It is content-based
  and by construction blind to a non-convergent loop that emits a different diff
  each cycle; a prior investigation checked all 24 gate verdicts across four
  runaway runs and found the duplicate flag false on every one. It also makes
  every such escalation report the detector instead of the cause.
- **Fix the accounting and stop there.** Leaves no ceiling for loops spanning two
  agentic stages, and — at today's default — quietly cuts one in seven successful
  implementation runs.
- **Derive the ceiling at runtime from cost telemetry.** Rejected: ingest is
  asynchronous and lossy (the audit found runs with empty telemetry status while
  the read model showed them terminal). A safety budget must be enforced from
  journal state the runner already holds.
- **Count repasses in the projector only, and leave enforcement alone.** Makes
  the waste visible without stopping it; the visibility half is worth doing
  regardless, which is why it is folded in here rather than deferred.

## Duplicate search

Searched 2026-08-08 against `Agent-Clubhouse/Goobers`, open and closed, terms:
`repass budget`, `MaxRepasses`, `bounded repass`, `repass_count`, `repass loop`,
`cost budget`, `token budget`, `model spend`, `per-run budget`, `budget
hierarchy`.

- **#1973** (open, `goobers:approved` + `goobers:critical`, no PR found) —
  `trackRepass` resets on pass and counters are per gate. This is the accounting
  defect and the closest neighbour. It asks for per-target-stage accounting; it
  does **not** propose a run-scoped ceiling across distinct stages, does not
  re-size the default for the corrected accounting, and does not touch
  projection. This draft is narrowed to that delta and is sequenced behind it.
- **#2272** (closed, shipped as `maxRunDuration`) — total-elapsed-time ceiling,
  explicitly "not a gate-evaluation-count or cycle-count metric." Different
  resource; composes.
- **#1671** (closed) — moved run-control budgets into the instance → gaggle →
  workflow override hierarchy. The mechanism `maxRunRepasses` would reuse, not a
  duplicate.
- **#1022** (open) — token/cost budgets at stage/run/workflow/gaggle/instance.
  Different axis; the two should ship as separate fields.
- **#364** (closed, durable per-PR repass budget + same-diff escalation), **#316**
  (closed, identical-diff non-convergence guard), **#2411** (open, surface the
  previous attempt to the agent) — adjacent loop-quality work; none bounds a run.
- **#953/#941** (per-cause remediation budgets), **#390** (per-PR repass budget),
  **#1698** (runControls stalled-run net) — per
  `domains/audit-issue-inventory.md:39`, none proposes a single all-causes
  declaration or a run ceiling.
- `repass_count` projection: no issue found under any term tried. Unfiled.

## Size and risk

**M.**

- Field, enforcement, and resume seeding are contained: `api/v1alpha1` (+ CRD and
  JSON-schema regeneration), `internal/runcontrol`, `internal/gate`,
  `internal/runner` resume reconstruction.
- The projection change touches the read-model contract. `repass_count` is
  currently constant zero, so populating it can only improve accuracy, but adding
  `rerun_count` is a schema migration — and read.db migrations apply by slice
  index, so it appends rather than renumbering. History backfills by
  reprojection; no journal changes.
- The default change is the real risk and the reason this wants design review
  rather than direct implementation: raising the per-stage default from 3 to 6
  doubles the worst-case agentic spend of a single-loop workflow. It must land
  together with the accounting fix. An instance that sets `maxRepasses`
  explicitly is unaffected — only the default moves.
- Blast radius: every gate evaluation in every workflow. Migration for config
  authors: none required; the new field is optional and absent means inherited.
- DSL 2.0 only.
