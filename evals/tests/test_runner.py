#!/usr/bin/env python3
"""Tests for evals/runner.py (#2667) — mock adapters only, no network.

Run with: python3 -m unittest discover -s evals/tests
"""
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from adapters.shim import AdapterShim, CassetteStore, ShadowRealModeForbiddenError  # noqa: E402
from judge_plugin_interface import JudgeKind  # noqa: E402
from runner import (  # noqa: E402
    Runner,
    SuiteValidationError,
    _adapter_mode,
    _build_judge_registry,
    load_suite,
    validate_suite,
)


def make_runner(**shim_kwargs) -> Runner:
    tmp = tempfile.mkdtemp()
    shim = AdapterShim(store=CassetteStore(root=tmp), run_id="test-run", **shim_kwargs)
    return Runner(adapter_shim=shim)


class ValidateSuiteTests(unittest.TestCase):
    def test_valid_suite_passes(self):
        validate_suite(
            {
                "suite_name": "s",
                "scenarios": [{"id": "a", "name": "A", "input": "hi"}],
            }
        )

    def test_missing_suite_name_rejected(self):
        with self.assertRaises(SuiteValidationError):
            validate_suite({"scenarios": [{"id": "a", "name": "A", "input": "hi"}]})

    def test_empty_scenarios_rejected(self):
        with self.assertRaises(SuiteValidationError):
            validate_suite({"suite_name": "s", "scenarios": []})

    def test_scenario_missing_required_field_rejected(self):
        with self.assertRaises(SuiteValidationError):
            validate_suite({"suite_name": "s", "scenarios": [{"id": "a", "name": "A"}]})

    def test_invalid_mode_rejected(self):
        with self.assertRaises(SuiteValidationError):
            validate_suite(
                {
                    "suite_name": "s",
                    "scenarios": [{"id": "a", "name": "A", "input": "hi", "mode": "bogus"}],
                }
            )

    def test_invalid_stage_type_rejected(self):
        with self.assertRaises(SuiteValidationError):
            validate_suite(
                {
                    "suite_name": "s",
                    "scenarios": [
                        {
                            "id": "a",
                            "name": "A",
                            "input": "hi",
                            "stages": [{"name": "x", "type": "bogus"}],
                        }
                    ],
                }
            )

    def test_load_suite_from_disk(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            f.write('{"suite_name": "s", "scenarios": [{"id": "a", "name": "A", "input": "hi"}]}')
            path = f.name
        suite = load_suite(path)
        self.assertEqual(suite["suite_name"], "s")


class AdapterModeTests(unittest.TestCase):
    def test_mode_field_preferred(self):
        self.assertEqual(_adapter_mode({"mode": "replay", "mock_type": "mock"}), "replay")

    def test_mock_type_fallback(self):
        self.assertEqual(_adapter_mode({"mock_type": "no-op"}), "no-op")

    def test_defaults_to_mock(self):
        self.assertEqual(_adapter_mode({}), "mock")

    def test_unknown_mode_rejected(self):
        with self.assertRaises(SuiteValidationError):
            _adapter_mode({"mode": "bogus"})


class JudgeRegistryBuildingTests(unittest.TestCase):
    def test_default_registry_has_exact_match_checker(self):
        registry = _build_judge_registry({})
        self.assertIn("exact-match", [p.judge_id for p in registry.all()])

    def test_default_registry_omits_checker_without_reference(self):
        registry = _build_judge_registry({}, has_reference=False)
        self.assertEqual(registry.all(), ())

    def test_prompt_template_adds_provisional_llm_judge(self):
        registry = _build_judge_registry({"prompt_template": "score {input}"})
        kinds = [p.kind for p in registry.all()]
        self.assertIn(JudgeKind.LLM, kinds)

    def test_explicit_plugins_list_builds_named_checkers(self):
        registry = _build_judge_registry({"plugins": ["exact-match", "similarity"]})
        ids = sorted(p.judge_id for p in registry.all())
        self.assertEqual(ids, ["exact-match", "similarity"])

    def test_unknown_plugin_without_prompt_template_raises(self):
        with self.assertRaises(SuiteValidationError):
            _build_judge_registry({"plugins": ["mystery-judge"]})


class RunnerDeterministicStageTests(unittest.TestCase):
    def test_deterministic_stage_is_reproducible(self):
        runner = make_runner()
        scenario = {
            "id": "s1",
            "name": "det",
            "input": {"x": 1},
            "stages": [{"name": "step", "type": "deterministic", "seed": 7}],
            "expected": None,
        }
        result_a = runner.run_scenario(scenario)
        result_b = runner.run_scenario(scenario)
        self.assertEqual(result_a.candidate_output(), result_b.candidate_output())

    def test_different_seed_changes_output(self):
        runner = make_runner()
        base = {"id": "s1", "name": "det", "input": {"x": 1}, "stages": []}
        r1 = runner.run_scenario({**base, "stages": [{"name": "s", "type": "deterministic", "seed": 1}]})
        r2 = runner.run_scenario({**base, "stages": [{"name": "s", "type": "deterministic", "seed": 2}]})
        self.assertNotEqual(r1.candidate_output(), r2.candidate_output())


class RunnerAgenticStageTests(unittest.TestCase):
    def test_mock_mode_uses_configured_response(self):
        runner = make_runner()
        scenario = {
            "id": "s1",
            "name": "agentic",
            "input": {"amount": 42},
            "stages": [
                {
                    "name": "call",
                    "type": "agentic",
                    "tool_mocks": {
                        "bank_api": {"mode": "mock", "response": {"status": 200, "body": {"ok": True}}}
                    },
                }
            ],
        }
        result = runner.run_scenario(scenario)
        self.assertEqual(result.candidate_output(), {"status": 200, "body": {"ok": True}})
        artifacts = result.artifacts()
        self.assertEqual(len(artifacts), 1)
        self.assertEqual(artifacts[0]["adapter_id"], "bank_api")
        self.assertEqual(artifacts[0]["mode"], "mock")
        self.assertIn("signature", artifacts[0])

    def test_no_op_mode_never_reaches_real_caller(self):
        called = []
        runner = make_runner(real_callers={"bank_api": lambda req: called.append(req) or {"status": 200}})
        scenario = {
            "id": "s1",
            "name": "agentic",
            "input": {},
            "stages": [
                {"name": "call", "type": "agentic", "tool_mocks": {"bank_api": {"mode": "no-op"}}}
            ],
        }
        result = runner.run_scenario(scenario)
        self.assertEqual(called, [])
        # EVALS_SANDBOX_API.md §3.2: no-op is a normal successful mode
        # selection, not a policy refusal — "blocked" is reserved for an
        # actual rejection (e.g. a shadow run's real request).
        self.assertEqual(result.artifacts()[0]["status"], "ok")

    def test_shadow_scenario_forces_no_op_instead_of_real(self):
        called = []
        runner = make_runner(real_callers={"bank_api": lambda req: called.append(req) or {"status": 200}})
        scenario = {
            "id": "s1",
            "name": "shadow-agentic",
            "mode": "shadow",
            "input": {},
            "stages": [
                {"name": "call", "type": "agentic", "tool_mocks": {"bank_api": {"mode": "real"}}}
            ],
        }
        result = runner.run_scenario(scenario)
        self.assertEqual(called, [], "shadow run must never reach a real adapter caller")
        self.assertEqual(result.artifacts()[0]["mode"], "no-op")

    def test_shim_independently_rejects_real_mode_under_shadow(self):
        # Layer 2 of EVALS_SANDBOX_API.md §6.1 rule 1's required double
        # enforcement: even if this runner's own pre-emption (the test
        # above) were ever bypassed or buggy, AdapterShim.invoke itself must
        # independently refuse mode="real" when shadow=True. Call the shim
        # directly, bypassing the runner's policy layer entirely, to prove
        # this is a real second layer and not just this runner trusting
        # itself twice.
        called = []
        shim = AdapterShim(
            store=CassetteStore(root=tempfile.mkdtemp()),
            real_callers={"bank_api": lambda req: called.append(req) or {"status": 200}},
        )
        with self.assertRaises(ShadowRealModeForbiddenError):
            shim.invoke("bank_api", "real", {"method": "POST", "path": "/x"}, shadow=True)
        self.assertEqual(called, [])

    def test_replay_missing_cassette_reports_error_status_not_crash(self):
        runner = make_runner()
        scenario = {
            "id": "s1",
            "name": "agentic",
            "input": {},
            "stages": [
                {"name": "call", "type": "agentic", "tool_mocks": {"bank_api": {"mode": "replay"}}}
            ],
        }
        result = runner.run_scenario(scenario)
        self.assertEqual(result.artifacts()[0]["status"], "error")

    def test_side_by_side_runs_both_baseline_and_candidate(self):
        runner = make_runner()
        scenario = {
            "id": "s1",
            "name": "sbs",
            "mode": "side-by-side",
            "input": {"x": 1},
            "stages": [{"name": "step", "type": "deterministic", "seed": 3}],
        }
        result = runner.run_scenario(scenario)
        self.assertIsNotNone(result.baseline_output())
        self.assertIsNotNone(result.candidate_output())

    def test_side_by_side_without_expected_does_not_spuriously_fail(self):
        # A baseline-vs-candidate comparison with no golden `expected` must
        # not auto-register a checker that can only ever say "no expected
        # value provided" and hard-fail via route_for_review's rule 1 for a
        # reason unrelated to candidate quality (regression: an earlier
        # version of this default used ExactMatchChecker unconditionally
        # whenever a baseline existed, which can never score without
        # `expected`).
        runner = make_runner()
        scenario = {
            "id": "s1",
            "name": "sbs-no-expected",
            "mode": "side-by-side",
            "input": {"x": 1},
            "stages": [{"name": "step", "type": "deterministic", "seed": 3}],
        }
        result = runner.run_scenario(scenario)
        self.assertNotEqual(result.review.verdict, "fail")

    def test_single_mode_has_no_baseline(self):
        runner = make_runner()
        scenario = {
            "id": "s1",
            "name": "single",
            "mode": "single",
            "input": {"x": 1},
            "stages": [{"name": "step", "type": "deterministic", "seed": 3}],
        }
        result = runner.run_scenario(scenario)
        self.assertIsNone(result.baseline_output())


class RunnerJudgeBreakdownTests(unittest.TestCase):
    def test_report_includes_per_judge_breakdown(self):
        runner = make_runner()
        scenario = {
            "id": "s1",
            "name": "judged",
            "input": "hello",
            "expected": "hello",
            "stages": [{"name": "step", "type": "deterministic", "seed": 1}],
            "judge": {"plugins": ["exact-match", "similarity"]},
        }
        result = runner.run_scenario(scenario)
        report = result.to_report_dict()
        self.assertEqual(len(report["judges"]), 2)
        judge_ids = {j["judge_id"] for j in report["judges"]}
        self.assertEqual(judge_ids, {"exact-match", "similarity"})
        for j in report["judges"]:
            self.assertIn("score", j)
            self.assertIn("reason", j)

    def test_ensemble_score_and_verdict_present(self):
        runner = make_runner()
        scenario = {
            "id": "s1",
            "name": "judged",
            "input": "hello",
            "expected": "hello",
            "stages": [{"name": "step", "type": "deterministic", "seed": 1}],
            "judge": {"plugins": ["exact-match"]},
        }
        result = runner.run_scenario(scenario)
        self.assertGreaterEqual(result.ensemble_score, 0.0)
        self.assertLessEqual(result.ensemble_score, 1.0)
        self.assertIn(result.review.verdict, ("pass", "fail", "human-review"))

    def test_schema_threshold_field_is_honored_as_pass_threshold(self):
        # eval_schema.json (as vendored) names this field `threshold`, not
        # `pass_threshold` — the runner must read the schema's real field
        # name, not only the extended-schema name #2664 proposed.
        runner = make_runner()
        scenario = {
            "id": "s1",
            "name": "thresholded",
            "input": "hello",
            "expected": "hello world",
            "stages": [{"name": "step", "type": "deterministic", "seed": 1}],
            "judge": {"plugins": ["similarity"], "threshold": 0.99},
        }
        result = runner.run_scenario(scenario)
        # "hello" vs "hello world" is similar but not similar enough to
        # clear a 0.99 threshold, so it must land below "pass".
        self.assertNotEqual(result.review.verdict, "pass")

    def test_provisional_llm_judge_routes_to_human_review(self):
        # Deterministic stages -> candidate == expected -> exact-match would
        # pass, but a prompt_template-only judge config adds the provisional
        # LLM stub, which is always low-confidence and must force
        # human-review rather than a silent pass.
        runner = make_runner()
        scenario = {
            "id": "s1",
            "name": "prompt-judged",
            "input": "hello",
            "stages": [{"name": "step", "type": "deterministic", "seed": 1}],
            "judge": {"prompt_template": "Is {candidate_output} reasonable for {input}?"},
        }
        result = runner.run_scenario(scenario)
        self.assertEqual(result.review.verdict, "human-review")


class RunSuiteReportTests(unittest.TestCase):
    def test_run_suite_produces_summary_and_per_scenario_reports(self):
        runner = make_runner()
        suite = {
            "suite_name": "demo",
            "scenarios": [
                {
                    "id": "a",
                    "name": "A",
                    "input": "x",
                    "expected": "deterministic:placeholder",
                    "stages": [{"name": "s", "type": "deterministic", "seed": 0}],
                    "judge": {"plugins": ["similarity"]},
                },
                {
                    "id": "b",
                    "name": "B",
                    "input": {"y": 2},
                    "stages": [
                        {
                            "name": "call",
                            "type": "agentic",
                            "tool_mocks": {"catalog": {"mode": "mock", "response": {"status": 200}}},
                        }
                    ],
                },
            ],
        }
        report = runner.run_suite(suite)
        self.assertEqual(report["suite_name"], "demo")
        self.assertEqual(len(report["scenarios"]), 2)
        self.assertEqual(report["summary"]["total"], 2)
        self.assertIn("by_verdict", report["summary"])
        # Every scenario report round-trips through JSON (report writer's
        # actual output format) without needing a custom encoder.
        import json

        json.dumps(report)


if __name__ == "__main__":
    unittest.main()
