# evals/

Design and implementation home for EvalSuite (deterministic, reproducible
evaluation of agentic workflows — [#2662](https://github.com/Agent-Clubhouse/Goobers/issues/2662)).

## Design docs

- [`EVALS_SANDBOX_API.md`](./EVALS_SANDBOX_API.md) — the tool-adapter API
  contract (`real` / `mock` / `replay` / `no-op` modes) and security
  guidelines for shadow/dark runs.
- [`EVALS_CASSETTE.md`](./EVALS_CASSETTE.md) — the cassette storage format
  that `replay` mode reads from and `real`+`record` sessions write to.

Other design docs for sibling child issues under the epic (DSL/schema, judge
harness, adapter shim prototype, CI gating) land here as their issues are
worked. Runner integration (#2667) is documented in the section below.

---

# EvalSuite runner (#2667)

Status: prototype / research artifact, same scope note as every other
`evals/` directory in this epic (#2662) — not wired into `make ci`, the Go
build, or any daemon. No dependency on the rest of the Go module; this whole
directory can be deleted without affecting anything else in the repo.

`runner.py` is #2667's deliverable: it loads an EvalSuite (`eval_schema.json`
— #2663), executes each scenario's stages, invokes tool adapters in their
configured mode through the adapter shim (#2666), invokes judge plugins
through the judge harness (#2664), and writes a report with a per-scenario,
per-judge score breakdown plus cassette artifact links.

## Vendored files — provisional, pending sibling merges

At the time this was written, #2663/#2664/#2665/#2666 all had real, tested
work on sibling branches, but none had merged to `main` yet. Rather than
guess at their interfaces or block on merge order, this PR vendors
byte-identical copies of exactly the files this runner needs, from these
commits:

| Vendored path | Source branch | Source commit |
|---|---|---|
| `evals/eval_schema.json` | `gritty-bear/2663-evals-dsl-schema-tests` | `839fd22f` |
| `evals/judge_plugin_interface.py` | `patient-wasp/2664-evals-judge-harness` | `50a89fbb` |
| `evals/judge_templates/` | `patient-wasp/2664-evals-judge-harness` | `50a89fbb` |
| `evals/tests/test_judge_plugin_interface_vendored.py` | `patient-wasp/2664-evals-judge-harness` | `50a89fbb` |
| `evals/adapters/shim.py`, `evals/adapters/__init__.py` | `noble-salmon/2666-evals-adapter-shim-prototype` | `89fadcab` |
| `evals/samples/mvp-evals.json` | `gritty-bear/2663-evals-dsl-schema-tests` | `839fd22f` |
| `evals/.gitignore`, `evals/adapters/cassettes/.gitignore` | `noble-salmon/2666-evals-adapter-shim-prototype` | `89fadcab` |

**Reconciliation, once the real PRs land:** these paths should be
overwritten by the actual merges, not coexist with two copies. If a
vendored file here is byte-identical to what merges, the merge is a no-op
diff for that file. If the sibling PR's file diverged during its own review
(e.g. #2665/#2671's `mode` vs `mock_type` field-naming reconciliation that
was in progress at the time of this PR — see `_adapter_mode()` below), take
the merged version and re-run `evals/tests/` — this runner reads the
sibling contracts structurally (attribute/field access), not by pinning to
a specific vendored revision, so it should keep passing against small
shape changes; anything that doesn't is a real integration bug worth
finding now rather than after both land silently out of sync.

`server.py` and `cli.py` from the adapter shim prototype were **not**
vendored — this runner talks to `AdapterShim` in-process (direct Python
import), not over HTTP, since "mock adapters are enough for your own
tests" per this issue's brief. Wiring the runner to the HTTP shim instead
(for a real out-of-process adapter deployment) is provisional/future work,
noted in `runner.py`'s module docstring.

## Provisional/documented gaps

1. **`eval_schema.json`'s `judge` object doesn't yet have `plugins`/
   `weights`/threshold-override fields.** #2664's design doc
   (`EVALS_JUDGE_DESIGN.md`, "EvalSuite metadata wiring") asks #2663 to add
   them; as vendored, the schema only has `prompt_template`/`threshold`.
   `runner.py` reads the extended fields when present (forward-compatible)
   but does not require them — see `_build_judge_registry`'s default
   fallback.
2. **No real LLM backend exists in this repo.** `ProvisionalPromptJudge` in
   `runner.py` implements the judge contract for `prompt_template`-only
   scenarios without calling a model — it returns a fixed, deliberately
   low-confidence result so `route_for_review`'s low-confidence rule routes
   it to human-review rather than the runner fabricating a score. Swapping
   in a real model client is a documented one-method follow-up
   (`_call_model`), not this issue's scope.
3. **`stages[].tool_mocks`' mode field name.** `EVALS_SANDBOX_API.md` names
   it `mode`; gritty-bear's own sample suite (`evals/samples/mvp-evals.json`,
   not vendored here) uses `mock_type` for the same purpose — an
   in-progress naming reconciliation between #2663 and #2665 as of this
   writing. `runner.py`'s `_adapter_mode()` accepts either key (preferring
   `mode`) so this runner works regardless of which name the schema settles
   on.
4. **Shadow-run adapter policy is enforced by the runner, provisionally.**
   `EVALS_SANDBOX_API.md` §6.1 rule 2 says a shadow run's adapter set
   defaults to `no-op` for any adapter with a non-empty `side_effects`
   manifest, unless the scenario explicitly pins a safer mode. No adapter
   registry/manifest endpoint exists yet in the prototype shim for the
   runner to query, so — as the conservative stand-in — this runner forces
   `no-op` for any stage that requests `mode: "real"` inside a shadow
   scenario, unconditionally (not manifest-aware). This is strictly
   *more* conservative than the spec (real is never allowed for shadow
   regardless of the adapter's actual side-effect profile), so it can only
   ever be too strict, never too permissive, until the manifest endpoint
   exists. `AdapterShim.invoke` also independently rejects real calls it
   receives directly (defense in depth, per the same section) —
   `evals/tests/test_runner.py::test_shadow_scenario_forces_no_op_instead_of_real`
   asserts a real caller is never reached.
5. **The implicit default judge is `ExactMatchChecker`, gated strictly on an
   explicit `expected` value — not `SimilarityChecker`, and not "baseline
   present."** Two things pushed this: (a) `SimilarityChecker` and
   `ExactMatchChecker` are both `kind=DETERMINISTIC` in the vendored
   `judge_plugin_interface.py`, and `route_for_review`'s rule 1 hard-fails a
   scenario on *any* deterministic score below 1.0 — so a graded similarity
   ratio (e.g. 0.83) in the default set would fail almost every
   non-identical candidate regardless of the suite's own `threshold`, which
   seems unlikely to be the intended default and is worth confirming with
   #2664/patient-wasp rather than silently working around; (b)
   `ExactMatchChecker` only ever reads `context.expected` (no baseline
   fallback), so it must not be auto-registered for a side-by-side scenario
   that has a baseline but no `expected` — that would hard-fail on "no
   expected value provided," a reason unrelated to candidate quality.
   Net effect: a scenario gets an implicit default checker only when it sets
   `expected`; a baseline-only side-by-side scenario with no `expected` gets
   no implicit deterministic checker (falls through to a provisional LLM
   judge if `prompt_template` is set, or "no judge configured" ->
   human-review otherwise). Graded similarity judging remains fully
   available via explicit opt-in (`judge.plugins: ["similarity"]`).
   See `_build_judge_registry`'s docstring in `runner.py` for the same note
   colocated with the code.

## Running it

```sh
# Run the tests (mock adapters only, no network):
python3 -m unittest discover -s evals/tests -v

# Run a suite:
python3 -m evals.runner --suite evals/samples/mvp-evals.json --out evals/runs
```

No third-party dependencies — stdlib only, matching every other prototype
in this epic.
