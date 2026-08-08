#!/usr/bin/env python3
"""Stdlib-only unit tests for evals/judge_plugin_interface.py.

Run with: python3 -m unittest discover -s evals/tests
No pytest dependency, matching the rest of the EvalSuite research
artifacts' "no external deps" convention (see run_evals.py).
"""

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from judge_plugin_interface import (  # noqa: E402
    ExactMatchChecker,
    JudgeContext,
    JudgeKind,
    JudgeResult,
    LLMJudgePlugin,
    RegexChecker,
    SimilarityChecker,
    compute_ensemble_score,
    route_for_review,
)


def _ctx(**overrides):
    base = dict(
        scenario_id="s1",
        input="summarize this",
        candidate_output="a summary",
        expected="a summary",
    )
    base.update(overrides)
    return JudgeContext(**base)


class ExactMatchCheckerTests(unittest.TestCase):
    def test_match_scores_one(self):
        result = ExactMatchChecker().evaluate(_ctx())
        self.assertEqual(result.score, 1.0)

    def test_mismatch_scores_zero(self):
        result = ExactMatchChecker().evaluate(_ctx(candidate_output="different"))
        self.assertEqual(result.score, 0.0)

    def test_missing_expected_is_zero_confidence(self):
        result = ExactMatchChecker().evaluate(_ctx(expected=None))
        self.assertEqual(result.confidence, 0.0)

    def test_a_real_assertion_is_strict(self):
        self.assertTrue(ExactMatchChecker().evaluate(_ctx()).strict)

    def test_missing_expected_abstention_is_not_strict(self):
        result = ExactMatchChecker().evaluate(_ctx(expected=None))
        self.assertFalse(result.strict)


class RegexCheckerTests(unittest.TestCase):
    def test_required_pattern_present(self):
        checker = RegexChecker(required_pattern=r"^a ")
        self.assertEqual(checker.evaluate(_ctx()).score, 1.0)

    def test_required_pattern_missing(self):
        checker = RegexChecker(required_pattern=r"^z ")
        self.assertEqual(checker.evaluate(_ctx()).score, 0.0)

    def test_forbidden_pattern_present(self):
        checker = RegexChecker(forbidden_pattern=r"\bsecret\b")
        result = checker.evaluate(_ctx(candidate_output="the secret plan"))
        self.assertEqual(result.score, 0.0)

    def test_requires_at_least_one_pattern(self):
        with self.assertRaises(ValueError):
            RegexChecker()


class SimilarityCheckerTests(unittest.TestCase):
    def test_identical_strings_score_one(self):
        result = SimilarityChecker().evaluate(_ctx())
        self.assertEqual(result.score, 1.0)

    def test_falls_back_to_baseline_when_no_expected(self):
        result = SimilarityChecker().evaluate(
            _ctx(expected=None, baseline_output="a summary", candidate_output="a summary")
        )
        self.assertEqual(result.score, 1.0)

    def test_no_reference_is_zero_confidence(self):
        result = SimilarityChecker().evaluate(_ctx(expected=None, baseline_output=None))
        self.assertEqual(result.confidence, 0.0)

    def test_is_never_strict_even_on_a_perfect_match(self):
        # A graded score is never a binary assertion, even at 1.0 — strict
        # is about the KIND of judgment being made, not this particular
        # result's value.
        self.assertFalse(SimilarityChecker().evaluate(_ctx()).strict)

    def test_near_miss_is_not_strict(self):
        result = SimilarityChecker().evaluate(_ctx(candidate_output="a summary!"))
        self.assertFalse(result.strict)
        self.assertGreater(result.score, 0.0)
        self.assertLess(result.score, 1.0)


class _StubLLMJudge(LLMJudgePlugin):
    """Minimal concrete LLMJudgePlugin for testing evaluate()'s response
    handling without a real model call."""

    def __init__(self, response, judge_id="stub-llm"):
        super().__init__(judge_id, prompt_template="{input}")
        self._response = response

    def _call_model(self, prompt):
        return self._response


class LLMJudgePluginConfidenceTests(unittest.TestCase):
    """Regression coverage for the fail-safe (not fail-open) confidence
    default: a model response that omits `confidence` must be treated as
    LOW confidence (routes to human review), not high confidence (skips
    the human-review gate route_for_review's low-confidence branch exists
    to enforce)."""

    def test_reported_confidence_is_used_as_is(self):
        result = _StubLLMJudge({"score": 0.9, "confidence": 0.95}).evaluate(_ctx())
        self.assertEqual(result.confidence, 0.95)

    def test_omitted_confidence_defaults_low_not_high(self):
        result = _StubLLMJudge({"score": 0.9}).evaluate(_ctx())
        self.assertEqual(result.confidence, 0.0)

    def test_omitted_confidence_triggers_human_review_despite_high_score(self):
        result = _StubLLMJudge({"score": 0.95}).evaluate(_ctx())
        decision = route_for_review([result])
        self.assertEqual(decision.verdict, "human-review")


class EnsembleScoreTests(unittest.TestCase):
    def test_default_weights_worked_example(self):
        # Matches the worked example in EVALS_JUDGE_DESIGN.md: deterministic
        # passes (1.0), LLM judge scores 0.75, classifier scores 0.9.
        results = [
            JudgeResult("exact-match", JudgeKind.DETERMINISTIC, 1.0, "matched"),
            JudgeResult("clarity-llm", JudgeKind.LLM, 0.75, "mostly clear"),
            JudgeResult("toxicity-cls", JudgeKind.CLASSIFIER, 0.9, "benign"),
        ]
        # 0.4*1.0 + 0.4*0.75 + 0.2*0.9 = 0.4 + 0.3 + 0.18 = 0.88, / total_weight 1.0
        self.assertAlmostEqual(compute_ensemble_score(results), 0.88)

    def test_missing_kind_worked_example_normalizes_by_weight_that_ran(self):
        # Second worked example in EVALS_JUDGE_DESIGN.md: no classifier judge
        # ran, so its 0.2 weight must not silently deflate the score — the
        # denominator excludes it too.
        results = [
            JudgeResult("exact-match", JudgeKind.DETERMINISTIC, 1.0, "matched"),
            JudgeResult("clarity-llm", JudgeKind.LLM, 0.75, "mostly clear"),
        ]
        # (0.4*1.0 + 0.4*0.75) / (0.4 + 0.4) = 0.70 / 0.8 = 0.875
        self.assertAlmostEqual(compute_ensemble_score(results), 0.875)

    def test_multiple_judges_of_one_kind_split_that_kinds_weight(self):
        results = [
            JudgeResult("llm-a", JudgeKind.LLM, 1.0, "ok"),
            JudgeResult("llm-b", JudgeKind.LLM, 0.5, "ok"),
        ]
        # Only LLM weight (0.4) is in play; it's split evenly across the two
        # LLM judges, so the ensemble score is just their unweighted mean.
        self.assertAlmostEqual(compute_ensemble_score(results), 0.75)

    def test_empty_results_raises(self):
        with self.assertRaises(ValueError):
            compute_ensemble_score([])

    def test_zero_weight_kinds_fall_back_to_unweighted_mean(self):
        results = [JudgeResult("cls-only", JudgeKind.CLASSIFIER, 0.6, "ok")]
        self.assertAlmostEqual(compute_ensemble_score(results, weights={"classifier": 0.0}), 0.6)


class RouteForReviewTests(unittest.TestCase):
    def test_deterministic_failure_always_fails(self):
        results = [JudgeResult("exact-match", JudgeKind.DETERMINISTIC, 0.0, "no match")]
        decision = route_for_review(results, ensemble_score=0.95)
        self.assertEqual(decision.verdict, "fail")

    def test_non_strict_deterministic_near_miss_does_not_hard_fail(self):
        # Regression: SimilarityChecker scoring 0.95 (a near-miss, not a
        # failed assertion) must not trip the "any deterministic failure ->
        # fail" rule the way a real ExactMatchChecker/RegexChecker failure
        # does. strict=False is exactly what SimilarityChecker sets to
        # signal "graded score, not a binary check" (found during #2667).
        results = [
            JudgeResult(
                "similarity", JudgeKind.DETERMINISTIC, 0.95, "close match", strict=False
            ),
            JudgeResult("clarity-llm", JudgeKind.LLM, 0.9, "clear", confidence=0.9),
        ]
        decision = route_for_review(results)
        self.assertEqual(decision.verdict, "pass")

    def test_non_strict_deterministic_abstention_does_not_hard_fail(self):
        # Regression: a checker with strict=False and score=0.0 because it
        # had nothing to compare against (no `expected`/`baseline_output`)
        # is an abstention, not an assertion of failure — the strict-bypass
        # rule must not fire for it. Isolated from ensemble math with an
        # explicit ensemble_score (same pattern as
        # test_deterministic_failure_always_fails above) because an
        # abstained judge's score=0.0 legitimately still dilutes the
        # ensemble average via the normal weighted-mean path below — that's
        # a separate, already-approved piece of math this test isn't about.
        results = [
            JudgeResult(
                "exact-match",
                JudgeKind.DETERMINISTIC,
                0.0,
                "no expected value",
                confidence=0.0,
                strict=False,
            ),
        ]
        decision = route_for_review(results, ensemble_score=0.9)
        self.assertEqual(decision.verdict, "pass")

    def test_high_score_passes(self):
        results = [JudgeResult("llm", JudgeKind.LLM, 0.9, "good", confidence=0.9)]
        decision = route_for_review(results)
        self.assertEqual(decision.verdict, "pass")

    def test_gray_zone_routes_to_human_review(self):
        results = [JudgeResult("llm", JudgeKind.LLM, 0.7, "borderline", confidence=0.9)]
        decision = route_for_review(results)
        self.assertEqual(decision.verdict, "human-review")

    def test_low_confidence_routes_to_human_review_even_if_score_is_high(self):
        results = [JudgeResult("llm", JudgeKind.LLM, 0.95, "confident-sounding", confidence=0.3)]
        decision = route_for_review(results)
        self.assertEqual(decision.verdict, "human-review")

    def test_low_score_fails(self):
        results = [JudgeResult("llm", JudgeKind.LLM, 0.2, "poor", confidence=0.9)]
        decision = route_for_review(results)
        self.assertEqual(decision.verdict, "fail")

    def test_safety_critical_forces_review_on_any_variance(self):
        results = [JudgeResult("llm", JudgeKind.LLM, 0.85, "mostly safe", confidence=0.9)]
        decision = route_for_review(results, safety_critical=True)
        self.assertEqual(decision.verdict, "human-review")

    def test_safety_critical_passes_only_above_the_raised_threshold(self):
        results = [JudgeResult("llm", JudgeKind.LLM, 0.97, "safe", confidence=0.95)]
        decision = route_for_review(results, safety_critical=True)
        self.assertEqual(decision.verdict, "pass")


if __name__ == "__main__":
    unittest.main()
