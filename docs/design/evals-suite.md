# Design: EvalSuite — end-to-end workflow evaluation

> Status: **historical — the #2662 direction, closed out and redirected**
> Epic: [#2662](https://github.com/Agent-Clubhouse/Goobers/issues/2662) (closed)
> Superseded-by (proposed): [#2681](https://github.com/Agent-Clubhouse/Goobers/issues/2681)
> — nominated, not yet approved
> Home directory: [`evals/`](../../evals/)

> **⚠️ Direction superseded — read this first.**
>
> **This page documents the #2662 EvalSuite direction, which is no longer the
> direction being pursued.** [#2681](https://github.com/Agent-Clubhouse/Goobers/issues/2681)
> redirects EvalSuite to a **native Go, DSL-driven, opt-in** capability built on
> the existing runner (`internal/localscheduler`), DSL (`api/schemas`), journal,
> and telemetry — not a parallel Python system under `evals/`. Every child of
> #2662 (#2663–#2669) was closed on 2026-08-08 as **redirected, not delivered**:
> the adapter shim (#2666) and runner integration (#2667) were closed pointing at
> #2681/#2682, and CI gating (#2668) was closed as premature before a comparison
> primitive exists to gate on.
>
> **Do not build against this page.** Start from #2681 and its children
> ([#2682](https://github.com/Agent-Clubhouse/Goobers/issues/2682) variant
> comparison, [#2683](https://github.com/Agent-Clubhouse/Goobers/issues/2683)
> eval-definition DSL,
> [#2684](https://github.com/Agent-Clubhouse/Goobers/issues/2684) Tutor
> integration).
>
> **Why this page still exists.** #2681 is `goobers:nominated`, not
> `goobers:approved`. Retiring `evals/`, `evals-gate.yml`, and the
> path-filtered `evals-tests.yml` waits on that ratification, so the artifacts
> and their records are preserved as a historical account rather than deleted
> ahead of a decision. The judge-template and sandbox research here remains
> useful reference; it is not an implementation plan.


| Child | What it covers | Status | Landed artifact |
|---|---|---|---|
| [#2663](https://github.com/Agent-Clubhouse/Goobers/issues/2663) | DSL & schema validation tests | Design + tests landed | [`evals/eval_schema.json`](../../evals/eval_schema.json), [`evals/tests/validate_schema.py`](../../evals/tests/validate_schema.py), [`evals/samples/`](../../evals/samples/) |
| [#2664](https://github.com/Agent-Clubhouse/Goobers/issues/2664) | Judge harness design & LLM prompt templates | Design + plugin contract landed, and #2667's runner does wire it (`evals/runner.py`); that runner is retained-but-unexecuted, not the direction being carried forward | [`evals/EVALS_JUDGE_DESIGN.md`](../../evals/EVALS_JUDGE_DESIGN.md), [`evals/judge_plugin_interface.py`](../../evals/judge_plugin_interface.py), [`evals/judge_templates/`](../../evals/judge_templates/) |
| [#2665](https://github.com/Agent-Clubhouse/Goobers/issues/2665) | Sandbox & tool-adapter API + cassette format | Design finalized; adapter shim implementation is #2666 | [`evals/EVALS_SANDBOX_API.md`](../../evals/EVALS_SANDBOX_API.md), [`evals/EVALS_CASSETTE.md`](../../evals/EVALS_CASSETTE.md) |
| [#2666](https://github.com/Agent-Clubhouse/Goobers/issues/2666) | Adapter shim prototype & cassette recorder | **Landed** (PR #2674, `91b48b521`, 2026-08-07), then **closed 2026-08-08 as redirected** — the prototype was built against a sandbox/adapter design not being carried forward; the runner's existing execution path covers this under #2681 | [`evals/adapters/`](../../evals/adapters/) (`shim.py`, `cli.py`, `server.py`, `cassettes/`) |
| [#2667](https://github.com/Agent-Clubhouse/Goobers/issues/2667) | Runner integration (judge harness + adapter wiring) | **Landed** (PR #2676, `1d0608195`, 2026-08-08), then **closed the same day as redirected** to [#2682](https://github.com/Agent-Clubhouse/Goobers/issues/2682), which generalizes `tutorholdout`'s existing before/after comparison instead of carrying the Python judge harness forward | [`evals/runner.py`](../../evals/runner.py), [`evals/tests/test_runner.py`](../../evals/tests/test_runner.py) |
| [#2668](https://github.com/Agent-Clubhouse/Goobers/issues/2668) | CI gating, baseline management & alerting | **Closed 2026-08-08, parked** — gating on eval results is premature before #2682/#2683 exist to gate on | — |
| [#2669](https://github.com/Agent-Clubhouse/Goobers/issues/2669) | Docs, onboarding, and review (this doc) | **Closed** — this page, now historical | This page |

## What EvalSuite is

EvalSuite is a deterministic, reproducible way to evaluate agentic workflows —
comparing a baseline against a candidate version (side-by-side / A/B) or
mirroring production-like input against a candidate without side effects
(shadow / dark runs) — so a workflow or gaggle change can be judged against
prior behavior before it ships, rather than only observed after the fact.

## Why this doc exists

This page was an index for #2662's children, each of which owned its own
substantive design doc. It is retained as the historical account of that
direction and the record of how each child was disposed of.

## What actually exists in the tree

- **Runs in CI:** the JSON-Schema DSL (`evals/eval_schema.json`), its sample
  suites, and their validation tests (`evals/tests/validate_schema.py`),
  exercised by the path-filtered `evals-tests.yml`. This is the only part of
  `evals/` any automation executes.
- **Present but unexecuted:** the judge plugin contract and its deterministic
  checkers (`evals/judge_plugin_interface.py`, `evals/judge_templates/`),
  unit-tested in isolation, plus the cassette and adapter-API design documents.
- **Built, then redirected — present but not the plan:** the end-to-end runner
  (`evals/runner.py`, PR #2676, 2026-08-08) and the adapter shim with its
  cassette recorder (`evals/adapters/`, PR #2674, 2026-08-07). Both landed and
  both are covered by their own Python tests; #2666 and #2667 were closed as
  **redirected the day after** the runner merged, not as unfinished. The code is
  retained pending #2681's ratification and is not executed by any automation.
- **Never built:** a CI gate that fails a PR on a regression. #2668 was closed
  as premature rather than delivered.
- **`evals-gate.yml` can never activate as written**, and not because the runner
  is missing. Its provisioning guard requires `evals/cmd/` and
  `evals/testdata/baselines/` — the shape of the *illustrative Go* CLI
  (`go run ./evals/cmd/evalsrun gate`) that `evals-ci-gating.md` sketched. The
  runner that actually shipped is Python and produces neither directory, so the
  guard's predicate is permanently unsatisfiable.

## Where to go next

- **Working on evaluation?** Go to
  [#2681](https://github.com/Agent-Clubhouse/Goobers/issues/2681) and its
  children, not to this page.
- **Running the retained code?** `evals/runner.py` and `evals/adapters/` work
  and have tests, but nothing in CI runs them and #2681 does not build on them.
  Treat them as reference, not as a foundation.
- **Reading the historical research?** The judge-ensemble math and
  human-review routing in
  [`EVALS_JUDGE_DESIGN.md`](../../evals/EVALS_JUDGE_DESIGN.md), and the
  shadow-run safety rules in §6 of
  [`EVALS_SANDBOX_API.md`](../../evals/EVALS_SANDBOX_API.md), are the parts
  #2681 explicitly calls useful reference. They are not an implementation
  plan for the new direction.
- **Running the surviving tests?** The
  [onboarding checklist](../guides/evals-onboarding.md) still describes them
  accurately.
- **Reviewing a PR that touches `evals/`?** The
  [review checklist](../guides/evals-review-checklist.md) still applies to
  changes within the retained tree.
