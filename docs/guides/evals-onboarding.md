# EvalSuite onboarding: running tests and reading reports

This is the checklist for getting an EvalSuite running locally and making
sense of what it produces. It covers the two things you need before you can
touch anything else: **run the existing suite**, and **read its report**.

For what EvalSuite is and why, see the [design overview](../design/evals-suite.md).
For what reviewers check before approving a PR that changes EvalSuite
artifacts, see the [review checklist](evals-review-checklist.md).

> EvalSuite is early-stage: schema validation, the judge harness, and the
> sandbox/adapter API are landing incrementally via
> [#2663](https://github.com/Agent-Clubhouse/Goobers/issues/2663)–[#2668](https://github.com/Agent-Clubhouse/Goobers/issues/2668).
> The steps below describe the pattern established during the research phase;
> if a command here doesn't match what's in the tree, the child issue that
> owns that piece is the source of truth — file a doc fix against this page.

## 1. Get the suite running locally

- [ ] Create and activate an isolated Python environment (don't install into
      system Python):
  ```sh
  python3 -m venv .venv
  source .venv/bin/activate
  ```
- [ ] Install the eval tooling's dependencies (`pytest`, `jsonschema`):
  ```sh
  pip install pytest jsonschema
  ```
- [ ] Validate the DSL schema and sample suite before running anything else —
      a broken schema makes every downstream failure misleading:
  ```sh
  pytest -q
  ```
- [ ] Run the deterministic prototype runner against a sample suite to
      produce a report (path and entry point land with
      [#2667](https://github.com/Agent-Clubhouse/Goobers/issues/2667); until
      then this mirrors the research-phase prototype's `run_evals.py`).
- [ ] Confirm CI runs the same `pytest` step you just ran locally — if your
      local result and CI disagree, suspect environment drift before
      suspecting the suite.

## 2. Read a report

Each scenario in a report carries a `mode` (`side-by-side` or `shadow`), a
`baseline` and `candidate` stage trace, and a `judge` verdict:

```json
{
  "id": "s1",
  "name": "simple-side-by-side",
  "mode": "side-by-side",
  "judge": { "score": 0.84, "pass": true },
  "baseline": { "stages": [ ... ] },
  "candidate": { "stages": [ ... ] }
}
```

- [ ] Check `judge.pass` first — it's the binary verdict against the suite's
      configured threshold (default `0.8`; safety-critical suites may set it
      higher). Don't eyeball `judge.score` alone against a remembered
      threshold.
- [ ] If `judge.pass` is `false`, diff `baseline.stages` against
      `candidate.stages` stage-by-stage — the first stage where outputs
      diverge is almost always the one worth reading closely; later
      divergence is often downstream noise from the first.
- [ ] A `shadow` mode scenario's candidate stages should show mocked or
      replayed tool output (e.g. a `mocked_tool` field), never a live
      side-effecting call — if you see what looks like a real API response
      in a shadow run, that's a sandboxing bug, not a suite failure. Report it
      against [#2665](https://github.com/Agent-Clubhouse/Goobers/issues/2665)
      / [#2666](https://github.com/Agent-Clubhouse/Goobers/issues/2666).
- [ ] Scores in the `[0.6, 0.8)` gray zone (or any judge-reported
      `confidence < 0.6`) are routed to human review, not treated as pass or
      fail — see the judge design doc landing with
      [#2664](https://github.com/Agent-Clubhouse/Goobers/issues/2664) for the
      full ensemble/threshold rules once it merges.
- [ ] A suite-wide regression (multiple scenarios newly failing against the
      same baseline) is a signal to check first whether the *baseline*
      changed underneath you, not only the candidate.

## 3. Writing your own scenario

- [ ] Start from an existing sample scenario and change one thing at a time —
      the schema is strict enough that a scenario with three simultaneous
      changes makes validation failures hard to attribute.
- [ ] Deterministic stages must be seedable/pure; anything that calls a tool
      belongs in an agentic stage with its tool dependency declared, so it can
      run in `mock`/`replay`/`no-op` mode during CI.
- [ ] Don't hand-write cassette files — record them through the adapter shim
      so signatures and PII scrubbing stay correct (see the review checklist's
      cassette section).
