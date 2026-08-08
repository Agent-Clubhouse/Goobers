# EvalSuite PR review checklist

Use this when reviewing a PR that touches EvalSuite artifacts: the DSL/schema,
sample or production suites, the judge harness, the sandbox/tool-adapter API,
cassettes, or the CI gate that runs any of it. It complements, not replaces,
the project's normal review bar.

New to EvalSuite? Read the [onboarding checklist](evals-onboarding.md) and the
[design overview](../design/evals-suite.md) first — several items below only
make sense with that context.

## 1. Determinism (MUST)

- [ ] Deterministic stages are actually deterministic — no unseeded
      randomness, wall-clock reads, or non-canonicalized map/JSON ordering
      leaking into a stage's output.
- [ ] Agentic stages declare their tool dependencies explicitly; nothing
      reaches a tool through an undeclared side channel that would go live in
      a `mock`/`replay`/`no-op` run.
- [ ] Any new source of non-determinism (timestamps, generated IDs, etc.) is
      normalized or replaced with a deterministic token before it can land in
      a cassette or a report.

## 2. Sandbox / shadow-run safety (MUST)

- [ ] A shadow (dark) run cannot perform a real side effect — mode
      `no-op`/`mock`/`replay` is enforced by the adapter, not just by
      convention in the calling stage.
- [ ] Adapters declare `side_effects` (db/email/payment/etc.) and the runner's
      policy enforcement actually blocks disallowed effects for the run mode,
      rather than only logging them.
- [ ] No production credentials or endpoints are reachable from a shadow or
      replay-mode run.

## 3. Cassettes (MUST if the PR touches cassette recording/storage)

- [ ] Sensitive fields (PII, auth tokens, payment identifiers) are scrubbed
      before a cassette is written — check the actual recorded fixture, not
      just the scrubbing code.
- [ ] Cassette signatures are computed from normalized request + seed, with
      volatile headers (e.g. `Date`, request IDs) excluded — a signature that
      changes on every recording defeats replay.
- [ ] Cassette writes are disabled in CI unless an explicit recording flag is
      set; CI should replay, never silently record.
- [ ] Cassettes are treated as immutable — an update creates a new
      signature/tag rather than mutating a file in place.

## 4. Judge changes (MUST if the PR touches judge prompts, weights, or thresholds)

- [ ] A changed prompt, weight, or threshold documents *why* — a threshold
      change is a policy decision, not a drive-by tuning fix, and needs the
      same scrutiny as a gate change elsewhere in the runner.
- [ ] The gray-zone / low-confidence routing to human review still triggers
      correctly after the change (don't let a threshold edit silently widen
      or collapse the human-review band).
- [ ] Judge output still matches the documented schema
      (`score`, `labels`, `reason`, `confidence`) — a judge that returns a
      differently-shaped object breaks every downstream consumer of the
      report, not just this suite.

## 5. Schema / DSL changes (MUST if the PR touches `eval_schema.json` or the suite DSL)

- [ ] Existing sample suites still validate against the updated schema, or
      are updated in the same PR — a schema change that breaks in-repo
      samples means the change is incomplete.
- [ ] Schema validation has a test that fails on the old schema and passes on
      the new one (not just "tests still pass").
- [ ] Backward-incompatible schema changes call out migration/versioning
      explicitly rather than silently orphaning existing suites.

## 6. CI gating (MUST if the PR touches the eval CI job or baseline management)

- [ ] A regression against the stored baseline actually fails the job — a
      gate that only warns is not a gate.
- [ ] Baseline updates are an explicit, reviewable action (a separate commit
      or flagged step), not an automatic side effect of a green run.
- [ ] The job's failure output tells a reviewer *which* scenario regressed
      and by how much, not just "eval gate failed."

## 7. Scope and hygiene (SHOULD)

- [ ] No secrets, tokens, or real connection strings in a committed cassette
      or fixture.
- [ ] New EvalSuite artifacts (schema, docs, samples) go where the design doc
      says they go, not scattered ad hoc — check
      [`docs/design/evals-suite.md`](../design/evals-suite.md) for the
      current landing spots.
- [ ] Docs (this checklist, the onboarding guide, the design overview) are
      updated in the same PR if the change makes any of them inaccurate —
      don't leave a follow-up doc PR as the only trace of a design decision.
