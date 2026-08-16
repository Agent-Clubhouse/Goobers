# Design: EvalSuite — end-to-end workflow evaluation

> Status: **draft — research and early implementation** (in progress)
> Epic: [#2662](https://github.com/Agent-Clubhouse/Goobers/issues/2662)
> Children: [#2663](https://github.com/Agent-Clubhouse/Goobers/issues/2663) DSL & schema tests ·
> [#2664](https://github.com/Agent-Clubhouse/Goobers/issues/2664) judge harness design ·
> [#2665](https://github.com/Agent-Clubhouse/Goobers/issues/2665) sandbox & adapter API ·
> [#2666](https://github.com/Agent-Clubhouse/Goobers/issues/2666) adapter shim prototype ·
> [#2667](https://github.com/Agent-Clubhouse/Goobers/issues/2667) runner integration ·
> [#2668](https://github.com/Agent-Clubhouse/Goobers/issues/2668) CI gating ·
> [#2669](https://github.com/Agent-Clubhouse/Goobers/issues/2669) docs & onboarding (this doc)

## What EvalSuite is

EvalSuite is a deterministic, reproducible way to evaluate agentic workflows —
comparing a baseline against a candidate version (side-by-side / A/B) or
mirroring production-like input against a candidate without side effects
(shadow / dark runs) — so a workflow or gaggle change can be judged against
prior behavior before it ships, rather than only observed after the fact.

## Why this doc exists, and why it's short

This page is deliberately an index, not the design itself. Each child issue
above owns its own substantive design doc (schema/tests, judge design, sandbox
and adapter API) as part of its own acceptance criteria. Duplicating that
content here would drift out of sync with what each child actually ships.
**As each child PR merges, this doc is updated to link the artifact or design
doc it landed** — schema location, judge harness design doc, sandbox/adapter
API doc, cassette format, and the runner entry point — instead of restating
their contents.

## Current state

The initial research pass (literature review, DSL/schema draft, sandbox and
judge design sketches, and a small deterministic prototype runner) exists as
preserved research artifacts and has not yet landed on `main`; the child
issues above track turning each piece into a reviewed, tested deliverable in
this repository. Until a given child merges, treat its design as a proposal,
not a contract.

## Where to go next

- Building or reviewing an EvalSuite? Start with the
  [onboarding checklist](../guides/evals-onboarding.md).
- Reviewing a PR that touches EvalSuite artifacts (schemas, cassettes, judge
  prompts, CI gates)? Use the
  [EvalSuite PR review checklist](../guides/evals-review-checklist.md).
