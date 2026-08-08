# EvalSuite onboarding: running tests and reading reports

This is the checklist for getting EvalSuite's current pieces running locally
and making sense of what they produce today. It covers what exists right
now: **validate a suite and run the unit tests**, and **read the judge
contract's output** — there is no end-to-end runner yet (that's
[#2667](https://github.com/Agent-Clubhouse/Goobers/issues/2667)), so "running
an EvalSuite scenario against real workflow output" isn't possible yet.

For what EvalSuite is and its overall status, see the
[design overview](../design/evals-suite.md). For what reviewers check before
approving a PR that changes EvalSuite artifacts, see the
[review checklist](evals-review-checklist.md).

## 1. Get the suite running locally

All EvalSuite tooling lives under [`evals/`](../../evals/) at the repo root.

- [ ] From the repo root, `cd evals` and create an isolated Python
      environment (don't install into system Python):
  ```sh
  cd evals
  python3 -m venv .venv
  source .venv/bin/activate
  ```
- [ ] Install the pinned dependencies:
  ```sh
  pip install -r requirements.txt
  ```
- [ ] Run the test suite — this validates `eval_schema.json` itself, every
      sample suite under `samples/` against it (including deliberately
      malformed fixtures that must be rejected), and the judge plugin
      contract's ensemble scoring / human-review routing:
  ```sh
  pytest
  ```
- [ ] Confirm CI runs the same command (`.github/workflows/evals-tests.yml`
      runs `pytest` from `evals/` on any push/PR touching that directory) —
      if your local result and CI disagree, suspect environment drift (Python
      version, stale `.venv`) before suspecting the suite.

There is no runner yet — `pytest` here validates the DSL and the judge
contract in isolation, it does not execute a scenario's stages against a
workflow. Adding a scenario means adding a new file under `evals/samples/`
and confirming it validates; it does not yet mean you can execute it.

## 2. The EvalSuite DSL, briefly

A suite (`evals/eval_schema.json`) is `suite_name` + one or more `scenarios`.
Each scenario has an `id`, `name`, `input`, and:

- `mode`: `side-by-side` (compare two versions), `shadow` (dark-run against
  real-shaped input, no side effects), `single` (evaluate one candidate
  alone — the default), or `synthetic` (labeled/synthetic input).
- `stages`: each `deterministic` (seeded, pure) or `agentic` (may call a
  tool). An agentic stage's `tool_mocks.<adapter_id>.mode` selects
  `real`/`mock`/`replay`/`no-op` for that adapter — see
  [`EVALS_SANDBOX_API.md`](../../evals/EVALS_SANDBOX_API.md).
- `judge`: `prompt_template` + `threshold` today; the judge harness design
  ([`EVALS_JUDGE_DESIGN.md`](../../evals/EVALS_JUDGE_DESIGN.md) §"EvalSuite
  metadata wiring") specifies an extended shape (`plugins`, `weights`,
  `pass_threshold`, `gray_zone_floor`, `low_confidence_floor`,
  `safety_critical`) that #2667's runner is expected to wire up.

Look at [`evals/samples/mvp-evals.json`](../../evals/samples/mvp-evals.json)
(a side-by-side and a shadow scenario) and
[`evals/samples/minimal-single.json`](../../evals/samples/minimal-single.json)
(the smallest valid document) before writing your own.

## 3. Reading judge output

There's no end-to-end report yet, but the judge contract itself
(`evals/judge_plugin_interface.py`) is implemented and unit-tested, and its
routing rules are what a report's verdict will be built on:

- [ ] A judge's raw result is `{score, labels, reason, confidence}`
      (0.0–1.0 for `score`/`confidence`) — a plugin returning an
      out-of-range value raises immediately rather than silently corrupting
      an ensemble average.
- [ ] `compute_ensemble_score` combines deterministic + LLM + classifier
      judges with default weights `0.4 / 0.4 / 0.2`, dividing by the weight
      of only the kinds that actually ran — a configured-but-absent
      classifier judge does not silently drag the score toward 0.
- [ ] `route_for_review`'s verdict, in priority order: (1) any *strict*
      deterministic checker failing (an exact-match or regex miss) is an
      unconditional `fail`; (2) on a `safety_critical` scenario, any judge
      scoring below `max(pass_threshold, 0.95)` forces `human-review`;
      (3) any LLM judge below the `low_confidence_floor` (default `0.6`)
      forces `human-review`; (4) an ensemble score in the gray zone
      (default `[0.6, 0.8)`) is `human-review`; (5) otherwise, ensemble
      score `>= pass_threshold` (default `0.8`) is `pass`, else `fail`.
- [ ] A *graded* checker's low score (e.g. `SimilarityChecker`) and any
      checker's abstention (nothing to compare against) are **not**
      unconditional failures — only a `strict=True` checker's sub-1.0 score
      is. If you see a scenario hard-failing on what should be a near-miss
      or an abstention, that's the `strict` flag misapplied, not a suite bug
      to work around.
- [ ] A `shadow`-mode scenario must never show a live tool response — every
      adapter invocation carries `metadata.shadow: true`, which the adapter
      shim and runner both independently reject for `mode: "real"`. If a
      shadow run's trace looks like it reached a real endpoint, that's a
      sandboxing bug (file against [#2666](https://github.com/Agent-Clubhouse/Goobers/issues/2666)),
      not something to route around in the suite.

## 4. Writing your own scenario

- [ ] Start from an existing sample scenario and change one thing at a time —
      the schema has `additionalProperties: false` throughout, so a scenario
      with several simultaneous changes makes a validation failure hard to
      attribute to the one that actually broke it.
- [ ] Deterministic stages must be seedable/pure; anything that calls a tool
      belongs in an agentic stage with its tool dependency declared via
      `tool_mocks`, so it can run in `mock`/`replay`/`no-op` mode in CI.
- [ ] Don't hand-write cassette files once the adapter shim exists — record
      them through it so signatures and PII scrubbing stay correct (see the
      review checklist's cassette section).
