# evals/

Design and implementation home for EvalSuite (deterministic, reproducible
evaluation of agentic workflows — [#2662](https://github.com/Agent-Clubhouse/Goobers/issues/2662)).

## Design docs

- [`EVALS_SANDBOX_API.md`](./EVALS_SANDBOX_API.md) — the tool-adapter API
  contract (`real` / `mock` / `replay` / `no-op` modes) and security
  guidelines for shadow/dark runs.
- [`EVALS_CASSETTE.md`](./EVALS_CASSETTE.md) — the cassette storage format
  that `replay` mode reads from and `real`+`record` sessions write to.

Other design docs for sibling child issues under the epic (judge harness,
adapter shim prototype, CI gating) land here as their issues are worked.
DSL/schema validation (#2663) and runner integration (#2667) are documented
in the sections below.

## DSL & schema validation (#2663)

- `eval_schema.json` — the EvalSuite DSL, as a JSON Schema (draft-07):
  required fields/types for a suite, its scenarios, their stages, and an
  optional judge block.
- `samples/*.json` — sample suites, each validated against the schema.
- `tests/validate_schema.py` — schema self-checks, sample-suite validation
  (parametrized over every file in `samples/`), and a set of deliberately
  malformed documents that must be rejected, so the runner fails fast on bad
  input rather than misbehaving downstream.

### Running the schema tests locally

```sh
cd evals
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
pytest
```

Adding a new sample suite: drop a new `*.json` file under `samples/` — it's
picked up automatically by the parametrized tests above, no test-code change
needed.

### CI

`.github/workflows/evals-tests.yml` installs the pinned dependencies and runs
`pytest` on every push/PR that touches this directory (test discovery covers
the whole `evals/` tree, not just `tests/` — sibling child issues land their
own test directories as they're implemented), failing the check on any
schema or sample-suite validation error.

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
| `evals/eval_schema.json` | `gritty-bear/2663-evals-dsl-schema-tests` | `c334399c` |
| `evals/judge_plugin_interface.py` | `patient-wasp/2664-evals-judge-harness` | `759111c7` |
| `evals/judge_templates/` | `patient-wasp/2664-evals-judge-harness` | `50a89fbb` |
| `evals/tests/test_judge_plugin_interface_vendored.py` | `patient-wasp/2664-evals-judge-harness` | `759111c7` |
| `evals/adapters/shim.py`, `evals/adapters/__init__.py` | `noble-salmon/2666-evals-adapter-shim-prototype` | `5ad66dce` |
| `evals/samples/mvp-evals.json` | `gritty-bear/2663-evals-dsl-schema-tests` | `c334399c` |
| `evals/.gitignore`, `evals/adapters/cassettes/.gitignore` | `noble-salmon/2666-evals-adapter-shim-prototype` | `5ad66dce` |

**Reconciliation, now that #2663 has landed:** `eval_schema.json` and
`samples/mvp-evals.json` above are confirmed byte-identical to what #2663
merged — this runner's vendored copies were already a no-op diff against
the real files, exactly as this section predicted before either side knew
which would merge first. This runner reads the sibling contracts
structurally (attribute/field access), not by pinning to a specific
vendored revision, so it should keep passing against small shape changes;
anything that doesn't is a real integration bug worth finding now rather
than after both land silently out of sync. One such change already happened
during this PR's own review: `eval_schema.json`'s
`stages[].tool_mocks.<adapter_id>` naming was resolved to `mode` (4-value
enum), matching the wire-level `/adapter/invoke` field exactly — re-synced
here (see `eval_schema.json`'s and `samples/mvp-evals.json`'s commit above)
after `_adapter_mode()` was already written to accept both `mode` and the
earlier `mock_type` name, so no runner code change was needed, only a
vendored-file re-sync. The `mock_type` fallback in `_adapter_mode()` is left
in place as harmless backward compatibility for any suite authored against
the earlier name. `judge_plugin_interface.py`, `judge_templates/`, and the
adapter shim files remain provisional pending #2664/#2666's own merges.

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
3. **`stages[].tool_mocks`' mode field name — resolved.** `EVALS_SANDBOX_API.md`
   named it `mode`; an earlier revision of gritty-bear's sample suite used
   `mock_type` for the same purpose. #2663/#2665/#2671 reconciled this to
   `mode` (4-value enum, matching the wire-level `/adapter/invoke` field
   exactly) partway through this PR's own review — vendored files re-synced
   to the resolved name. `runner.py`'s `_adapter_mode()` still accepts the
   earlier `mock_type` key as a harmless fallback.
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
   exists.

   **Genuinely two-layer as of the `5ad66dce` shim resync** (an earlier
   revision of this doc claimed this before the vendored shim actually
   implemented its half — corrected after vivid-gazelle's review of #2676
   caught the gap): `AdapterShim.invoke` now independently rejects
   `mode="real"` when `shadow=True` by raising
   `ShadowRealModeForbiddenError` (`SHADOW_REAL_MODE_FORBIDDEN`), *before*
   this runner's pre-emption existed, this shim call had no `shadow`
   parameter at all and could not have enforced anything — so the original
   claim was describing the spec's intended design, not this vendored
   copy's actual behavior. Both layers are independently exercised:
   `evals/tests/test_runner.py::test_shadow_scenario_forces_no_op_instead_of_real`
   asserts the runner's own pre-emption (layer 1, via the full
   `Runner.run_scenario` path), and
   `::test_shim_independently_rejects_real_mode_under_shadow` calls
   `AdapterShim.invoke` directly — bypassing the runner's policy layer
   entirely — to prove layer 2 holds on its own.
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

   **Confirmed as a real bug (a), fixed upstream, now vendored:**
   patient-wasp's `759111c7` (patient-wasp/2664-evals-judge-harness, vendored
   into this PR) adds `JudgeResult.strict` (default `True`) — only a
   `strict=True` deterministic failure hard-fails via rule 1;
   `SimilarityChecker` now always sets `strict=False`, and both checkers'
   abstention paths (no reference to compare against) do too. This resolves
   (a) at the source rather than requiring a runner-side workaround, but (b)
   (`ExactMatchChecker` has no baseline fallback) is unrelated to `strict`
   and still holds regardless — the `has_reference`-gated default above is
   left as-is in this PR rather than widened back to include
   `SimilarityChecker`, since reducing the default judge set's scope isn't
   this resync's purpose; revisiting it is reasonable future work now that
   the hard-fail risk is gone.

## Running it

```sh
# Run the tests (mock adapters only, no network):
python3 -m unittest discover -s evals/tests -v

# Run a suite:
python3 -m evals.runner --suite evals/samples/mvp-evals.json --out evals/runs
```

No third-party dependencies — stdlib only, matching every other prototype
in this epic.
