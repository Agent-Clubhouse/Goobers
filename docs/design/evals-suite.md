# Design: EvalSuite — end-to-end workflow evaluation

> Status: **draft — design finalized for the DSL, judge harness, and
> sandbox/adapter API; runner, adapter shim, and CI gating still pending**
> (in progress)
> Epic: [#2662](https://github.com/Agent-Clubhouse/Goobers/issues/2662)
> Home directory: [`evals/`](../../evals/)

| Child | What it covers | Status | Landed artifact |
|---|---|---|---|
| [#2663](https://github.com/Agent-Clubhouse/Goobers/issues/2663) | DSL & schema validation tests | Design + tests landed | [`evals/eval_schema.json`](../../evals/eval_schema.json), [`evals/tests/validate_schema.py`](../../evals/tests/validate_schema.py), [`evals/samples/`](../../evals/samples/) |
| [#2664](https://github.com/Agent-Clubhouse/Goobers/issues/2664) | Judge harness design & LLM prompt templates | Design + plugin contract landed; runner wiring is #2667 | [`evals/EVALS_JUDGE_DESIGN.md`](../../evals/EVALS_JUDGE_DESIGN.md), [`evals/judge_plugin_interface.py`](../../evals/judge_plugin_interface.py), [`evals/judge_templates/`](../../evals/judge_templates/) |
| [#2665](https://github.com/Agent-Clubhouse/Goobers/issues/2665) | Sandbox & tool-adapter API + cassette format | Design finalized; adapter shim implementation is #2666 | [`evals/EVALS_SANDBOX_API.md`](../../evals/EVALS_SANDBOX_API.md), [`evals/EVALS_CASSETTE.md`](../../evals/EVALS_CASSETTE.md) |
| [#2666](https://github.com/Agent-Clubhouse/Goobers/issues/2666) | Adapter shim prototype & cassette recorder | Not started | — |
| [#2667](https://github.com/Agent-Clubhouse/Goobers/issues/2667) | Runner integration (judge harness + adapter wiring) | Not started | — |
| [#2668](https://github.com/Agent-Clubhouse/Goobers/issues/2668) | CI gating, baseline management & alerting | Not started | — |
| [#2669](https://github.com/Agent-Clubhouse/Goobers/issues/2669) | Docs, onboarding, and review (this doc) | Landed; updated as siblings ship | This page |

## What EvalSuite is

EvalSuite is a deterministic, reproducible way to evaluate agentic workflows —
comparing a baseline against a candidate version (side-by-side / A/B) or
mirroring production-like input against a candidate without side effects
(shadow / dark runs) — so a workflow or gaggle change can be judged against
prior behavior before it ships, rather than only observed after the fact.

## Why this doc exists, and why it's short

This page is deliberately an index, not the design itself. Each child issue
owns its own substantive design doc as part of its own acceptance criteria —
duplicating that content here would drift out of sync with what each child
actually ships. **As each child PR merges, the table above is updated to
point at the artifact it landed**, and this section's prose is corrected if a
design decision changed shape during implementation (see
[`evals/README.md`](../../evals/README.md) for the directory's own running
summary of what's landed).

## Current state

- **Locked in and safe to build against:** the DSL (`eval_schema.json` —
  scenario `mode` is one of `side-by-side`, `shadow`, `single`, `synthetic`;
  a stage's `tool_mocks.<adapter_id>.mode` selects `real`/`mock`/`replay`/
  `no-op`), the judge ensemble math and human-review routing thresholds
  (`compute_ensemble_score`, `route_for_review` — see
  [`EVALS_JUDGE_DESIGN.md`](../../evals/EVALS_JUDGE_DESIGN.md)), and the
  cassette format and adapter wire contract (see
  [`EVALS_SANDBOX_API.md`](../../evals/EVALS_SANDBOX_API.md) and
  [`EVALS_CASSETTE.md`](../../evals/EVALS_CASSETTE.md)).
- **Not real yet:** there is no runner that loads an EvalSuite end-to-end, no
  adapter shim process, and no CI gate that fails a PR on a regression. The
  judge plugin contract and its deterministic checkers are implemented and
  unit-tested in isolation (`evals/judge_plugin_interface.py`), but nothing
  wires them to an actual suite run yet — that's #2666/#2667/#2668.
- Until #2667 exists, "running an EvalSuite" means schema-validating it
  (`evals/tests/validate_schema.py`) and running the judge/ensemble unit
  tests in isolation — not executing scenarios against real or replayed
  workflow output. See the [onboarding checklist](../guides/evals-onboarding.md)
  for exactly what that looks like today.

## Where to go next

- Building or reviewing an EvalSuite? Start with the
  [onboarding checklist](../guides/evals-onboarding.md).
- Reviewing a PR that touches EvalSuite artifacts (schemas, cassettes, judge
  prompts, CI gates)? Use the
  [EvalSuite PR review checklist](../guides/evals-review-checklist.md).
- Contributing to #2666/#2667/#2668? Read
  [`EVALS_JUDGE_DESIGN.md`](../../evals/EVALS_JUDGE_DESIGN.md)'s "Runner
  interface summary" and [`EVALS_SANDBOX_API.md`](../../evals/EVALS_SANDBOX_API.md)'s
  §6 shadow-run rules first — both were written to hand the runner issue a
  concrete contract instead of a prose spec to re-derive.
