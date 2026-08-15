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

Rules below are from [`EVALS_SANDBOX_API.md`](../../evals/EVALS_SANDBOX_API.md)
§6 — read it in full if the PR touches adapter or runner policy code.

- [ ] `mode: "real"` is rejected whenever `metadata.shadow: true`, and
      independently in **two** places — the runner's policy layer (before
      the call leaves the runner process) and the adapter shim itself. A PR
      that removes either check because "the other one already covers it" is
      removing intentional redundancy, not dead code.
- [ ] A shadow run's adapter set defaults to `no-op` for any adapter with a
      non-empty `side_effects` manifest, with no implicit fallback to `real`
      — the suite author must explicitly pin an adapter to `mock`/`replay`
      to override this.
- [ ] Shadow invocations get a credential-free environment and no network
      egress to production/staging hosts — this is a second, independent
      barrier, not just the policy check above.
- [ ] A blocked or denied `real` call during a shadow run is logged to the
      audit trail at `warn` or higher, not silently absorbed — an adapter
      response of `status: "blocked"` is a distinct, expected outcome, not a
      transport error to be swallowed or retried.
- [ ] No production credentials or endpoints are reachable from a shadow or
      replay-mode run.

## 3. Cassettes (MUST if the PR touches cassette recording/storage)

Rules below are from [`EVALS_CASSETTE.md`](../../evals/EVALS_CASSETTE.md) —
read it in full for a cassette-format or recorder-behavior change.

- [ ] Sensitive fields (PII, auth tokens, payment identifiers) are scrubbed
      **before** a cassette is written, per the adapter's declared scrub
      rules — check the actual recorded fixture, not just the scrubbing
      code, and confirm `scrubbed_fields` is populated whenever scrubbing
      occurred.
- [ ] Cassette signatures are computed from canonicalized request (sorted
      keys, volatile headers like `Date`/`Request-Id`/`Traceparent`
      stripped) + seed (appended separately, not folded into the request) —
      a signature that changes on every recording defeats replay.
- [ ] Cassette writes are disabled in CI by default; `recorder_mode: "record"`
      requires an explicit, separate opt-in flag from `mode: "real"` — the
      two must not be conflated, and no suite-gating CI job sets the record
      flag.
- [ ] A missing cassette in `replay` mode fails fast (`CASSETTE_NOT_FOUND`)
      rather than falling back to a live call or a synthesized response —
      don't let a PR "fix" a replay failure by adding a silent fallback.
- [ ] Cassettes are immutable — an update creates a new file (new signature,
      or an explicit `superseded_signature` rotation tag) rather than
      mutating or deleting the old one in place.
- [ ] Cassettes recorded from real production-adjacent interactions live in
      access-controlled storage, never committed to the repo, regardless of
      scrubbing.

## 4. Judge changes (MUST if the PR touches judge prompts, weights, or thresholds)

Routing rules below are from
[`EVALS_JUDGE_DESIGN.md`](../../evals/EVALS_JUDGE_DESIGN.md) — read it in
full for the worked ensemble examples before reviewing a change to
`compute_ensemble_score` or `route_for_review`.

- [ ] A changed prompt, weight, or threshold documents *why* — a threshold
      change is a policy decision, not a drive-by tuning fix, and needs the
      same scrutiny as a gate change elsewhere in the runner.
- [ ] `route_for_review`'s priority order is preserved: strict-checker
      failure → safety-critical dissent (`< max(pass_threshold, 0.95)`) →
      low-confidence LLM judge (`< low_confidence_floor`, default `0.6`) →
      gray-zone ensemble score (default `[0.6, 0.8)`) → pass/fail on
      `pass_threshold` (default `0.8`). Don't let a refactor silently
      reorder or short-circuit an earlier rule.
- [ ] A new or changed checker sets `JudgeResult.strict` correctly —
      `strict=True` only for a genuine binary assertion (exact-match/regex)
      on a real evaluation. A graded score (e.g. similarity) or an
      abstention (nothing to compare against) must be `strict=False`, or it
      will unconditionally hard-fail scenarios it should instead be scored
      or excluded from evaluation entirely.
- [ ] `compute_ensemble_score` divides by the weight of only the judge kinds
      that actually ran for a scenario — a configured-but-absent kind (e.g.
      no classifier judge) must not silently drag the score toward 0. If a
      PR changes this math, check it against both worked examples in
      `EVALS_JUDGE_DESIGN.md` (all kinds present; one kind absent).
- [ ] Judge output still matches the documented schema
      (`score`, `labels`, `reason`, `confidence`, each `score`/`confidence`
      validated to `[0.0, 1.0]`) — a judge that returns a differently-shaped
      or out-of-range object breaks every downstream consumer, not just this
      suite.

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
- [ ] New EvalSuite artifacts (schema, docs, samples, code) land under
      [`evals/`](../../evals/), not scattered ad hoc — check
      [`docs/design/evals-suite.md`](../design/evals-suite.md) and
      [`evals/README.md`](../../evals/README.md) for the current layout.
- [ ] Docs (this checklist, the onboarding guide, the design overview) are
      updated in the same PR if the change makes any of them inaccurate —
      don't leave a follow-up doc PR as the only trace of a design decision.
