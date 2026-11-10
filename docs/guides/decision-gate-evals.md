# Benchmarking System One decisions against an LLM-judge baseline

Shadow mode shows whether the decision model agrees with deterministic ground
truth. This guide adds the missing comparison: System One versus a free-form
LLM judge asked the same question on identical inputs, so enforce-mode
graduation rests on counts rather than anecdotes. See also the
[A/B harness](decision-gate-ab-harness.md), which measures gate on versus off.

The harness is `internal/decisioneval`. It is offline and deterministic by
default, reuses the real `decisiongate.Gate` over any `decider.Decider`, and
never influences a run.

## Run it

```text
go run ./internal/decisioneval/cmd/decisioneval -fixture internal/decisioneval/testdata/claims_bad_input.json
```

The report lists, per arm on the report split: sample sizes, false passes
(missed positives), false blocks (flagged negatives), recall, precision,
false-alarm rate, unsure rate, Brier calibration (System One only), run-to-run
determinism over repeated asks, and System One versus judge agreement. It ends
with a shadow-to-enforce graduation verdict. Any percentage whose denominator
is below `-min-samples` (default 20) prints as `n/a`.

## Benchmark design per question

| Question | Label source | Minimum per class (report split) |
| --- | --- | --- |
| claims_bad_input | deterministic handoffcheck result, no human label | 20 positives, 20 negatives |
| refusal detection, failure-cause, PR-vs-diff, review severity, duplicate detection | two independent human labels, disagreements adjudicated | 20 per class, 50 recommended |

- **Fixture format:** one JSON file per yes/no question with `question`,
  `instructions` and `items`. Each item has `id`, `split` (`tune` or
  `report`), `text`, `label`, and `source`.
- **Splits:** the accept threshold is chosen on `tune` items only
  (`ChooseThreshold`), and all reported numbers use `report` items only. Never
  move an item between splits after seeing its result.
- **Refresh:** add newly observed shadow samples each release, keep ids stable,
  and re-run the report; retire items whose inputs no longer reflect the
  workflow.
- **Replay arms:** items carry recorded `systemOne` probabilities and `judge`
  verdicts (`yes`, `no`, `uncertain`), so the report is reproducible without
  credentials. The bundled fixture is synthetic and illustrates the format; it
  is not evidence.

## Live arms (opt-in)

Live calls are off unless `GOOBERS_DECIDE_EVAL_LIVE=1` (`LiveEnabled`). Library
callers pass their own `decider.Decider` and `Judge` built from their own
configuration; endpoints, keys and tenant details must come from environment
variables or secret stores and are never committed. Latency and cost per
decision are only observable on live arms and are not part of the offline
report.

## Graduation rule

`Report.Graduation` returns ready only when both classes meet the minimum
sample, System One's false-block rate is at most 2% of negatives, and its
recall is at least the baseline judge's. Treat it as necessary evidence, not a
substitute for review.

## Knowledge reranking (shadow)

`internal/knowledgerank` shadow-ranks already-eligible knowledge candidates
with the gate's score question (`JudgeScore`: irrelevant, related, directly
needed, scored 0 to 2). It is advisory: `Ranker.Shadow` returns a record next
to the selection the caller keeps using and never changes it. Code decides
eligibility first; the model only judges relevance.

- **Off by default.** `Settings.Enabled` is false, and a disabled or nil
  `Ranker` scores nothing.
- **Internal knowledge needs its own opt-in.** A candidate marked `Internal`
  is never sent to the model unless `AllowInternalKnowledge` is true, because
  the decider may be a third-party endpoint. `Sources` limits scoring to an
  allowlist; only the first `TopN` (default 10) eligible candidates are scored.
- **Fallback is inclusion.** Only a scored `irrelevant` item (score at or below
  `DropAtOrBelow`, default 0.5) is a would-drop. A borderline score, a model
  error, a timeout, or an unscored candidate stays in the hypothetical
  selection.
- **Reproducible records.** Each record has a task digest, the model version
  the decider reported, `TopN`, both thresholds, per-candidate score,
  confidence and class, the existing order, the hypothetical ranking and the
  would-drops. It holds no knowledge or task text.

Print the offline precision-at-k report for a labelled fixture:

```text
go run ./internal/decisioneval/cmd/decisioneval -rank-fixture internal/decisioneval/testdata/knowledge_rank.json
```

Tasks carry a graded `label` per candidate (0 irrelevant, 1 related, 2
directly needed), a `split`, and recorded `score` values for replay. The drop
threshold is chosen on `tune` tasks only (the highest cut that drops no needed
item, never above 0.95), and all numbers use `report` tasks. The report compares
the existing order with the model ranking on precision at 1, 3 and 5 for
directly needed items, with hit and slot counts, and counts needed items the
model would drop. Percentages print only when there are at least
`-min-samples` report tasks (default 20). The bundled fixture is synthetic and
illustrates the format; it is not evidence. Enforce is not implemented and
needs a real labelled sample first.
## Reviewer-finding scope check (shadow)

`decisiongate.Gate.ScopeCheck` records, per reviewer finding, whether it looks
in scope for the change. It is advisory and never edits a verdict: the library
returns `ScopeRecord` values and acting on them is a later, separate step.
Nothing calls it until a stage opts in, and it needs the gate enabled
(`decisionGate.mode: shadow`).

Code decides first, in this order:

1. A `critical` finding is in scope. It is never a would-demote.
2. A `rebase-needed` or `cross-pr-blocked` category is a would-demote: the
   implementer cannot fix either.
3. A finding whose file, or a path in its text, is in the diff is in scope.

Only the remainder goes to the model, as a noul question behind the
`review_finding_in_scope` threshold (default accept 0.8, reject 0.1, so only a
confident out-of-scope answer is a would-demote). An uncertain answer, a model
error, a timeout, or a finding past `MaxScopeModelCalls` (20 per verdict) is
`unsure` and leaves the verdict untouched. Records hold a digest, the code
facts (including `unchangedFromPrior` and `outsideDiff`), outcome, probability
and cache flag, error text, and an optional hand `reference` label. They never
hold the finding text or the criteria.

Print counts and rates from a JSON-lines file of records:

```text
go run ./internal/decisiongate/cmd/decisionscope -records scope-records.jsonl
```

Percentages print only when the denominator is at least `-min-sample` (default
30); below that they read `n/a`. Enforce (demoting out-of-scope findings so
they stop forcing a repass) is not implemented and needs measured agreement
against a hand-labeled sample first.
