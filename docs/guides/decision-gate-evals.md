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
