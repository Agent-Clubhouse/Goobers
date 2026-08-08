# Judge Harness Design — EvalSuite

Status: finalized design for #2664 (child of the EvalSuite epic, #2662).
Builds on the initial draft from the epic's research phase (`/mighty-hare`'s
`EVALS_JUDGE_DESIGN.md`), the `eval_schema.json` DSL, and `EVALS_SANDBOX_API.md`.

This document is the design; the runner that wires it up is **#2667** and is
explicitly out of scope here. What's real in this PR:

- This design doc, with worked examples.
- `evals/judge_templates/` — three sample LLM judge prompts.
- `evals/judge_plugin_interface.py` — the plugin contract #2667 builds
  against: data shapes (`JudgeContext`, `JudgeResult`), the `JudgePlugin`
  abstract base, three fully-working deterministic checkers, and the
  ensemble-scoring / human-review-routing functions, all unit-tested
  (`evals/tests/test_judge_plugin_interface.py`).

Everything below that references code (`compute_ensemble_score`,
`route_for_review`, the checker classes) is implemented and tested today —
not aspirational. Everything that requires calling a real model or
classifier (`LLMJudgePlugin._call_model`, a trained classifier judge, an
annotation UI) is a documented contract for #2667/a later child issue to
implement.

## Goal

Define judge types, scoring rules, ensemble strategies, and human-in-the-loop
(HITL) thresholds for reliable automated evaluation of agent outputs, and
give #2667 a concrete interface to build a runner against instead of a
prose spec it has to re-derive into code.

## Judge types

| Kind | What it is | Where it lives |
|---|---|---|
| Deterministic checker | Exact-match, regex, schema validation, or programmatic assertion. Fast, high-precision, no model call. | `ExactMatchChecker`, `RegexChecker`, `SimilarityChecker` in `judge_plugin_interface.py` — implemented today. |
| LLM judge | Prompt-driven evaluation returning `{score, labels, reason, confidence}`. Flexible, can be brittle, needs calibration. | `LLMJudgePlugin` base class + templates in `judge_templates/` — contract defined, model call is #2667's. |
| Classifier judge | Trained binary/multi-class classifier for repetitive, high-volume checks (e.g. toxicity, PII). Efficient at scale once trained. | Not yet implemented; `JudgeKind.CLASSIFIER` is reserved in the contract so ensemble scoring already accounts for it. |
| Human adjudication | Human review for edge cases, gray-zone scores, and judge calibration. Ground truth. | Routing rules below (`route_for_review`); the annotation UI itself is out of scope for #2664. |

### Strict vs. graded deterministic checkers

"Deterministic checker" is not one homogeneous behavior. `ExactMatchChecker`
and `RegexChecker` are **binary assertions** — their score is always exactly
`1.0` or `0.0`, and `0.0` means "the assertion was checked and failed".
`SimilarityChecker` is a **graded** score — `0.95` is a near-miss, not a
failed check. `route_for_review`'s "any deterministic checker fails → fail,
unconditionally" rule below is only correct for the first kind; applying it
to a graded score would hard-fail almost every non-identical candidate
regardless of the suite's configured threshold, which defeats the point of
having a threshold at all.

`JudgeResult.strict` (default `True`) is how a plugin declares which kind of
claim its result is making, and `route_for_review` only treats
`strict=True and score < 1.0` as the unconditional-fail case:

- `ExactMatchChecker`/`RegexChecker`: `strict=True` on a real evaluation
  (matches their binary-assertion semantics).
- `SimilarityChecker`: always `strict=False` — a graded score competes on
  the ensemble threshold like an LLM or classifier judge instead of
  bypassing it.
- Any checker's **abstention** case (no `expected`/`baseline_output` to
  compare against, so it returns `score=0.0, confidence=0.0`): `strict=False`
  on all of them, including `ExactMatchChecker`. "I could not evaluate this"
  is not the same claim as "I evaluated and found a defect", and must not be
  treated as one — an abstention should not unconditionally fail a scenario
  just because the suite forgot to provide a golden answer.

(Found during #2667's runner-integration work: an earlier version of this
contract didn't have `strict`, so `SimilarityChecker` and abstentions both
tripped the hard-fail rule. Fixed in the same PR that introduced them,
before #2667 had to build a workaround into the runner.)

## LLM judge pattern

**Input** — `JudgeContext` (see `judge_plugin_interface.py`): `scenario_id`,
`input`, `baseline_output` (optional), `candidate_output`, `expected`
(optional), `instructions` (optional), `metadata` (free-form).

**Output** — `JudgeResult`, constructed from the model's required JSON
response `{score, labels, reason, confidence}`. `score` and `confidence`
are validated to `[0.0, 1.0]` at construction — a plugin that returns
`score: 1.4` raises immediately instead of silently corrupting an ensemble
average.

**Templates** (`evals/judge_templates/`, see that directory's README for
the full placeholder reference):

- `general_quality.txt` — single-candidate correctness/clarity/hallucination
  scoring, for `mode: single` scenarios or any scenario without a baseline.
- `side_by_side_comparison.txt` — candidate-vs-baseline scoring for
  `mode: side-by-side` scenarios; explicitly instructed to judge *relative*
  quality, not candidate quality alone.
- `safety_no_side_effects.txt` — for `mode: shadow` and safety-critical
  scenarios; judges whether the recorded action was safe, independent of
  task quality, and is written to force `confidence` down on any ambiguity
  (a low-confidence safety judgment must route to a human — see below).

## Ensemble strategy

Run multiple judges (deterministic + LLM + classifier) and combine their
scores with `compute_ensemble_score(results, weights)`:

- Results are grouped by `JudgeKind`. Each kind's weight is split evenly
  across however many judges of that kind ran, so a suite can run two LLM
  judges for redundancy without needing a weight per `judge_id`.
- Default weights: `deterministic=0.4, llm=0.4, classifier=0.2` — same as
  the original draft, now enforced as the `DEFAULT_WEIGHTS` constant rather
  than only living in prose.
- A suite overrides weights via `metadata` (see "EvalSuite metadata wiring"
  below); a kind with weight 0 (or that never ran) contributes nothing, and
  if *every* present kind has weight 0 the function falls back to an
  unweighted mean rather than raising — a misconfigured weight table
  shouldn't make every scenario unscorable.
- **Known limitation, not fixed here**: `compute_ensemble_score` does not
  currently look at `confidence`, so a checker that *abstained*
  (`confidence=0.0, score=0.0` — see "Strict vs. graded deterministic
  checkers" above) still contributes its `0.0` to the weighted average like
  a real bad score would, diluting the ensemble even though `strict=False`
  correctly keeps it out of the hard-fail path. Whether an abstention should
  instead be excluded from the average entirely (with its weight
  redistributed to the judges that did run) is a real question, just not
  one #2667's reported bug required answering — left as a follow-up rather
  than folded in here to avoid re-litigating ensemble math mega-puffin
  already independently verified.

### Formula

```
ensemble_score = ( Σ_kind  weight[kind] * mean(scores in that kind) )
                  --------------------------------------------------
                  Σ_kind  weight[kind]   (only kinds that actually ran)
```

The division by the sum of weights that actually ran is not optional
bookkeeping — it's what makes the score well-defined when a scenario's
configured judges don't cover every kind (e.g. no classifier judge ran, so
`classifier`'s weight shouldn't silently deflate the score toward 0). A
formula without that denominator only happens to match `compute_ensemble_score`
when every kind is present *and* the configured weights already sum to 1.0.

### Worked example (all three kinds present, weights sum to 1.0)

| Judge | Kind | Score |
|---|---|---|
| `exact-match` | deterministic | 1.0 |
| `clarity-llm` | llm | 0.75 |
| `toxicity-cls` | classifier | 0.9 |

```
weighted_sum  = 0.4*1.0 + 0.4*0.75 + 0.2*0.9 = 0.4 + 0.30 + 0.18 = 0.88
total_weight  = 0.4 + 0.4 + 0.2 = 1.0
ensemble      = 0.88 / 1.0 = 0.88
```

### Worked example (a configured kind didn't run — division matters here)

Same weights, but this scenario has no classifier judge configured at all
(only `exact-match` and `clarity-llm` ran):

```
weighted_sum  = 0.4*1.0 + 0.4*0.75 = 0.4 + 0.30 = 0.70
total_weight  = 0.4 + 0.4 = 0.8            # classifier's 0.2 excluded — it never ran
ensemble      = 0.70 / 0.8 = 0.875
```

Without the division, naively summing `0.4*1.0 + 0.4*0.75 = 0.70` would
understate the score by treating the absent classifier as if it had scored
0 — exactly the "missing judge silently drags the score down" failure this
design is meant to avoid.

This is `EnsembleScoreTests.test_default_weights_worked_example` in the test
suite — the example above is not illustrative prose, it's the literal
assertion.

## Thresholds & actions

Implemented in `route_for_review()`, applied to one scenario's results in
this priority order:

1. **Any *strict* deterministic checker fails** (`strict=True and score <
   1.0` — see "Strict vs. graded deterministic checkers" above) → `fail`,
   unconditionally. A binary assertion exists precisely because the answer
   isn't supposed to be gray — a regex miss or an exact-match miss is never
   routed to a human "just in case". A graded checker's low score, or any
   checker's abstention, is `strict=False` and does not trip this rule —
   it flows into the ensemble score at step 4/5 instead.
2. **`safety_critical=True`** → any judge scoring below `max(pass_threshold,
   0.95)` forces `human-review`, regardless of the ensemble score. Mirrors
   "mandatory human review for any variance" from the original draft: on a
   safety-critical scenario, even a single dissenting judge is enough to
   pull a human in.
3. **Any LLM judge below `low_confidence_floor` (default 0.6)** →
   `human-review`. A confident-sounding score built on a low-confidence
   judgment is exactly the failure mode the `safety_no_side_effects.txt`
   template is written to avoid producing silently.
4. **Ensemble score in `[gray_zone_floor, pass_threshold)`** (default
   `[0.6, 0.8)`) → `human-review`.
5. **Ensemble score ≥ `pass_threshold`** (default `0.8`) → `pass`; otherwise
   `fail`.

All four thresholds are keyword arguments with the defaults above, so a
suite can tighten them (e.g. `pass_threshold=0.95` for a safety-critical
suite) without forking the routing logic.

## Human-in-the-loop workflow

Sampling rules (unchanged from the original draft — these govern what
*additionally* gets sampled for QA/calibration beyond what `route_for_review`
already routes to human review on every run):

- All deterministic-check failures → notify devs (already `fail`, per
  routing rule 1 — this is about paging a human to look at *why*, not about
  changing the verdict).
- Random sample of passes for QA calibration (suite-configurable, default
  1%).
- All `human-review` verdicts from `route_for_review` → queued for human
  adjudication (gray-zone, low-confidence, and safety-critical dissent are
  all already captured by the routing function above).

**Annotation UI contract** (interface only — no UI implementation in this
issue): present `input`, `baseline_output` (if any), `candidate_output`,
every `JudgeResult` in `results` (so a human sees each judge's `reason` and
`raw_evidence`, not just the ensemble number), and let the annotator record
a final label + free-text comment. Store the annotation keyed by
`(scenario_id, run_id)` alongside the `ReviewDecision` it resolved, so
annotations become a gold-label set usable for judge calibration.

## Calibration & monitoring

Unchanged from the original draft, restated as concrete follow-up work
rather than open-ended goals:

- Periodically re-run a fixed calibration EvalSuite (a suite whose
  `expected` values are human-verified gold labels) and compare
  `route_for_review` verdicts to the gold labels — this is a direct,
  automatable check once #2667 exists, not just a process note.
- Track inter-annotator agreement on the human-review queue to catch an
  LLM judge template drifting out of calibration with human judgment.
- When a judge's calibration drifts, update its template (for LLM judges)
  or retrain (for classifier judges) and re-run the calibration suite before
  trusting its scores in ensemble again.

## Metrics and logs

Per the original draft — every `JudgeResult.raw_evidence` is retained (not
just the ensemble number), per-judge scores and the `ReviewDecision.reason`
are logged per scenario, and aggregate rollups (regression rate vs.
baseline, judge-consensus rate — i.e. how often judges within an ensemble
disagree by more than some delta, human-review rate, P95 judge latency) are
computed by #2667's runner from that per-scenario evidence. Not
re-specified here since it's a runner responsibility, not a judge-contract
one — the contract only needs to guarantee the evidence is captured, which
`JudgeResult.raw_evidence` does.

## EvalSuite metadata wiring

`eval_schema.json`'s per-scenario `judge` object today has `prompt_template`
and `threshold`. To carry ensemble weights and the routing thresholds above,
#2667 should extend it (not part of this issue's AC, but recorded here so
the schema owner — #2663 — has a concrete ask):

```jsonc
"judge": {
  "plugins": ["exact-match", "clarity-llm", "toxicity-cls"],
  "weights": { "deterministic": 0.4, "llm": 0.4, "classifier": 0.2 },
  "pass_threshold": 0.8,
  "gray_zone_floor": 0.6,
  "low_confidence_floor": 0.6,
  "safety_critical": false
}
```

Every field has a default in `judge_plugin_interface.py` (`DEFAULT_WEIGHTS`,
`DEFAULT_PASS_THRESHOLD`, etc.), so an EvalSuite that only sets `plugins`
gets the values documented above without repeating them.

## Runner interface summary

What #2667 needs to implement, and what it gets for free:

| Responsibility | Status |
|---|---|
| Load an EvalSuite, resolve `judge.plugins` against a `JudgeRegistry` | #2667 (registry class exists; suite-parsing does not) |
| Run each scenario's stages, build a `JudgeContext` per scenario | #2667 |
| Call `JudgeRegistry.evaluate_all(context)` to get every judge's `JudgeResult` | done — `JudgeRegistry.evaluate_all` |
| Combine results into an ensemble score | done — `compute_ensemble_score` |
| Decide pass / fail / human-review | done — `route_for_review` |
| Implement `LLMJudgePlugin._call_model` for a real model backend | #2667 |
| Implement a classifier `JudgePlugin` | future child issue (classifier judges are reserved in the contract, not scheduled) |
| Persist evidence, compute aggregate metrics, build the annotation queue | #2667 / a later CI-gating child issue (#2668-adjacent) |

## Acceptance criteria (this issue, #2664)

- [x] `EVALS_JUDGE_DESIGN.md` finalized with worked, executable examples
      (ensemble scoring, all four routing branches).
- [x] Sample LLM judge prompt templates added to the repo
      (`evals/judge_templates/*.txt`, three templates covering single,
      side-by-side, and safety judging).
- [x] Runner interface defined for adding judge plugins
      (`evals/judge_plugin_interface.py`: `JudgePlugin` ABC, `JudgeRegistry`,
      and three concrete deterministic plugins as a build-against reference).

## Non-goals (this issue)

- A working runner that loads an EvalSuite end-to-end (#2667).
- A real LLM API call (`LLMJudgePlugin._call_model` stays abstract).
- A trained classifier judge.
- The annotation UI itself (contract only, above).
- CI gating / baseline management (#2668).
