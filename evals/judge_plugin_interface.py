#!/usr/bin/env python3
"""Judge plugin interface — EvalSuite (#2664).

This module defines the CONTRACT a judge plugin must satisfy so #2667's
runner integration can load, register, and score against any judge without
special-casing its kind. It intentionally does not implement a runner, an
LLM client, or a classifier: those are #2667's job. What's real here (pure
functions/classes, no I/O, no network) is:

  - JudgeContext / JudgeResult: the data shapes every judge consumes/produces.
  - JudgePlugin: the abstract base every judge kind subclasses.
  - ExactMatchChecker, RegexChecker, SimilarityChecker: fully working
    deterministic checkers (safe to implement now — no external calls).
  - LLMJudgePlugin: an abstract base that fixes the prompt/response contract
    for LLM-backed judges; concrete subclasses (which call a real model) are
    #2667's responsibility. See evals/judge_templates/ for the prompts they
    should send.
  - compute_ensemble_score / route_for_review: the ensemble and
    human-review-routing math from EVALS_JUDGE_DESIGN.md, implemented and
    unit-tested so #2667 does not have to re-derive it from prose.

Kept dependency-free (stdlib only) so it stays portable across the
throwaway sandboxes EvalSuite runs evaluate against.
"""

from __future__ import annotations

import difflib
import re
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Dict, List, Optional, Sequence


class JudgeKind(str, Enum):
    """The four judge types from EVALS_JUDGE_DESIGN.md."""

    DETERMINISTIC = "deterministic"
    LLM = "llm"
    CLASSIFIER = "classifier"
    HUMAN = "human"


@dataclass(frozen=True)
class JudgeContext:
    """Everything a judge needs to score one scenario run.

    Mirrors the "LLM judge pattern" input shape in EVALS_JUDGE_DESIGN.md so
    deterministic, LLM, and classifier judges all consume the same object —
    a judge that doesn't need `expected` or `instructions` just ignores them.
    """

    scenario_id: str
    input: Any
    candidate_output: Any
    baseline_output: Optional[Any] = None
    expected: Optional[Any] = None
    instructions: Optional[str] = None
    # Free-form extras a specific judge plugin may need (e.g. a classifier's
    # feature flags, or an LLM judge's model/temperature override) without
    # widening this shared shape for every future judge.
    metadata: Dict[str, Any] = field(default_factory=dict)


@dataclass(frozen=True)
class JudgeResult:
    """A single judge's verdict on one JudgeContext.

    `score` and `confidence` are always normalized to [0.0, 1.0] so
    compute_ensemble_score can combine results from judges of different
    kinds without per-kind special-casing.
    """

    judge_id: str
    kind: JudgeKind
    score: float
    reason: str
    labels: List[str] = field(default_factory=list)
    confidence: float = 1.0
    # A DETERMINISTIC result is only treated as a hard-fail gate (route_for_
    # review's "any deterministic failure -> fail, unconditionally" rule) when
    # `strict=True`. True for a binary pass/fail assertion (ExactMatchChecker,
    # RegexChecker: score is always exactly 0.0 or 1.0, and 0.0 means "the
    # assertion was checked and failed"). False for a graded/continuous
    # deterministic score (SimilarityChecker: 0.95 is a near-miss, not a
    # failed assertion — it should compete on the ensemble threshold like any
    # other judge) and for any checker's "I could not evaluate this" case
    # (e.g. no `expected` value was provided) — an abstention is not the same
    # claim as "I evaluated and found a defect", and must not be treated as
    # one. Only DETERMINISTIC kind results are ever consulted for this; LLM/
    # classifier/human results ignore the field entirely.
    strict: bool = True
    # Raw, unprocessed judge output (LLM completion text, classifier logits,
    # the exact regex match) — kept for audit trails per EVALS_JUDGE_DESIGN.md
    # ("Persist full judge evidence and LLM rationale for auditing").
    raw_evidence: Optional[Any] = None

    def __post_init__(self) -> None:
        if not 0.0 <= self.score <= 1.0:
            raise ValueError(f"{self.judge_id}: score {self.score!r} outside [0.0, 1.0]")
        if not 0.0 <= self.confidence <= 1.0:
            raise ValueError(f"{self.judge_id}: confidence {self.confidence!r} outside [0.0, 1.0]")


class JudgePlugin(ABC):
    """Base class every judge (deterministic, LLM, classifier, human-proxy)
    implements. #2667's runner discovers plugins by `judge_id` (see
    JudgeRegistry below) and calls `evaluate()` uniformly — it never needs to
    know whether a given judge_id is a regex check or an LLM call.
    """

    #: Stable identifier used in EvalSuite metadata (ensemble weights,
    #: thresholds) and in JudgeResult.judge_id. Must be unique within a
    #: JudgeRegistry.
    judge_id: str
    kind: JudgeKind

    @abstractmethod
    def evaluate(self, context: JudgeContext) -> JudgeResult:
        """Score one scenario run. Must be side-effect-free with respect to
        the scenario under evaluation (no mutating candidate/baseline state)
        — shadow-run safety depends on judges being pure observers.
        """
        raise NotImplementedError


class ExactMatchChecker(JudgePlugin):
    """Deterministic checker: candidate_output == expected, verbatim."""

    kind = JudgeKind.DETERMINISTIC

    def __init__(self, judge_id: str = "exact-match") -> None:
        self.judge_id = judge_id

    def evaluate(self, context: JudgeContext) -> JudgeResult:
        if context.expected is None:
            return JudgeResult(
                judge_id=self.judge_id,
                kind=self.kind,
                score=0.0,
                reason="no `expected` value provided; exact-match cannot score",
                confidence=0.0,
                # Abstention, not a failed assertion: route_for_review must
                # not hard-fail a scenario just because this checker had
                # nothing to compare against.
                strict=False,
            )
        matched = context.candidate_output == context.expected
        return JudgeResult(
            judge_id=self.judge_id,
            kind=self.kind,
            score=1.0 if matched else 0.0,
            reason="candidate matches expected exactly" if matched else "candidate differs from expected",
            raw_evidence={"expected": context.expected, "candidate": context.candidate_output},
            # strict=True (the default): a real exact-match assertion was
            # checked, so a 0.0 here means route_for_review should hard-fail.
        )


class RegexChecker(JudgePlugin):
    """Deterministic checker: candidate_output (stringified) matches a
    required pattern, and optionally must not match a forbidden one — e.g.
    "the response must not leak a raw account number" as a forbidden pattern.
    """

    kind = JudgeKind.DETERMINISTIC

    def __init__(
        self,
        judge_id: str = "regex-check",
        required_pattern: Optional[str] = None,
        forbidden_pattern: Optional[str] = None,
    ) -> None:
        if not required_pattern and not forbidden_pattern:
            raise ValueError("RegexChecker needs at least one of required_pattern/forbidden_pattern")
        self.judge_id = judge_id
        self._required = re.compile(required_pattern) if required_pattern else None
        self._forbidden = re.compile(forbidden_pattern) if forbidden_pattern else None

    def evaluate(self, context: JudgeContext) -> JudgeResult:
        text = str(context.candidate_output)
        if self._required and not self._required.search(text):
            return JudgeResult(
                judge_id=self.judge_id,
                kind=self.kind,
                score=0.0,
                reason=f"missing required pattern {self._required.pattern!r}",
                raw_evidence={"candidate": text},
            )
        if self._forbidden and self._forbidden.search(text):
            return JudgeResult(
                judge_id=self.judge_id,
                kind=self.kind,
                score=0.0,
                reason=f"matched forbidden pattern {self._forbidden.pattern!r}",
                raw_evidence={"candidate": text},
            )
        return JudgeResult(
            judge_id=self.judge_id,
            kind=self.kind,
            score=1.0,
            reason="satisfied required/forbidden pattern constraints",
            raw_evidence={"candidate": text},
        )


class SimilarityChecker(JudgePlugin):
    """Deterministic checker: normalized string/JSON similarity between
    candidate and expected (or baseline, if no expected is given — useful
    for regression scenarios that only assert "candidate ~= baseline").
    Same ratio() approach run_evals.py's prototype already uses, promoted
    here to the plugin contract so it composes with the ensemble scorer.

    Unlike ExactMatchChecker/RegexChecker, this is a GRADED score, not a
    binary assertion — 0.95 is a near-miss, not a failed check. Every
    JudgeResult this returns has `strict=False` so route_for_review's
    unconditional deterministic-hard-fail rule leaves it to compete on the
    ensemble score/threshold like an LLM or classifier judge, instead of
    hard-failing any candidate that isn't byte-for-byte identical.
    """

    kind = JudgeKind.DETERMINISTIC

    def __init__(self, judge_id: str = "similarity") -> None:
        self.judge_id = judge_id

    def evaluate(self, context: JudgeContext) -> JudgeResult:
        reference = context.expected if context.expected is not None else context.baseline_output
        if reference is None:
            return JudgeResult(
                judge_id=self.judge_id,
                kind=self.kind,
                score=0.0,
                reason="no `expected` or `baseline_output` to compare against",
                confidence=0.0,
                strict=False,
            )
        ratio = difflib.SequenceMatcher(a=_stringify(context.candidate_output), b=_stringify(reference)).ratio()
        return JudgeResult(
            judge_id=self.judge_id,
            kind=self.kind,
            score=ratio,
            reason=f"sequence similarity ratio={ratio:.3f}",
            raw_evidence={"candidate": context.candidate_output, "reference": reference},
            strict=False,
        )


def _stringify(value: Any) -> str:
    if isinstance(value, (dict, list)):
        import json

        return json.dumps(value, sort_keys=True)
    return str(value)


class LLMJudgePlugin(JudgePlugin):
    """Base for prompt-driven judges. Fixes the contract described in
    EVALS_JUDGE_DESIGN.md's "LLM judge pattern": a prompt template rendered
    from JudgeContext, sent to a model, and a required {score, labels,
    reason, confidence} JSON response.

    #2667 implements `_call_model`; this base class owns prompt rendering
    and response validation so every LLM judge parses/validates identically
    regardless of which model backs it.
    """

    kind = JudgeKind.LLM

    def __init__(self, judge_id: str, prompt_template: str) -> None:
        self.judge_id = judge_id
        self.prompt_template = prompt_template

    def render_prompt(self, context: JudgeContext) -> str:
        """Fill the prompt template's `{field}` placeholders from the
        context. Templates in evals/judge_templates/ use scenario_id, input,
        baseline_output, candidate_output, expected, and instructions.
        """
        return self.prompt_template.format(
            scenario_id=context.scenario_id,
            input=context.input,
            baseline_output=context.baseline_output,
            candidate_output=context.candidate_output,
            expected=context.expected,
            instructions=context.instructions or "",
        )

    @abstractmethod
    def _call_model(self, prompt: str) -> Dict[str, Any]:
        """#2667 implements this: send `prompt` to an LLM and return the
        parsed JSON response. Must return a dict with at least a `score`
        key; `labels` and `reason` are optional. `confidence` is optional
        too, but an omitted `confidence` is treated as "the model didn't
        report one" and defaults to 0.0, NOT 1.0 — see `evaluate()` below
        for why that direction matters.
        Not implemented here — no model client exists yet in this repo.
        """
        raise NotImplementedError

    def evaluate(self, context: JudgeContext) -> JudgeResult:
        prompt = self.render_prompt(context)
        response = self._call_model(prompt)
        return JudgeResult(
            judge_id=self.judge_id,
            kind=self.kind,
            score=float(response["score"]),
            reason=str(response.get("reason", "")),
            labels=list(response.get("labels", [])),
            # Fail-SAFE, not fail-open: route_for_review() sends any LLM
            # judge below low_confidence_floor to human review specifically
            # to catch shaky judgments. Defaulting a missing `confidence` to
            # 1.0 (maximally confident) would let a model that omits it —
            # or returns malformed JSON a caller patches over with `{}` —
            # silently skip that gate as "fully trustworthy". Defaulting to
            # 0.0 instead guarantees an omitted confidence routes to a
            # human rather than past one.
            confidence=float(response.get("confidence", 0.0)),
            raw_evidence=response,
        )


class JudgeRegistry:
    """Maps judge_id -> JudgePlugin instance. #2667's runner builds one of
    these per EvalSuite from the suite's `judge.plugins` config and passes it
    to compute_ensemble_score alongside each judge's weight.
    """

    def __init__(self) -> None:
        self._plugins: Dict[str, JudgePlugin] = {}

    def register(self, plugin: JudgePlugin) -> None:
        if plugin.judge_id in self._plugins:
            raise ValueError(f"judge_id {plugin.judge_id!r} already registered")
        self._plugins[plugin.judge_id] = plugin

    def get(self, judge_id: str) -> JudgePlugin:
        return self._plugins[judge_id]

    def all(self) -> Sequence[JudgePlugin]:
        return tuple(self._plugins.values())

    def evaluate_all(self, context: JudgeContext) -> List[JudgeResult]:
        return [plugin.evaluate(context) for plugin in self._plugins.values()]


# --- Ensemble scoring & human-review routing --------------------------------
#
# These implement the "Ensemble strategy" and "Thresholds & actions" sections
# of EVALS_JUDGE_DESIGN.md directly: pure functions, no I/O, so #2667 can
# unit-test its runner against them instead of re-deriving the math from
# prose, and so this design doc's examples are executable rather than
# aspirational.

DEFAULT_WEIGHTS: Dict[str, float] = {
    JudgeKind.DETERMINISTIC.value: 0.4,
    JudgeKind.LLM.value: 0.4,
    JudgeKind.CLASSIFIER.value: 0.2,
}

DEFAULT_PASS_THRESHOLD = 0.8
DEFAULT_GRAY_ZONE_FLOOR = 0.6
DEFAULT_LOW_CONFIDENCE_FLOOR = 0.6


def compute_ensemble_score(
    results: Sequence[JudgeResult],
    weights: Optional[Dict[str, float]] = None,
) -> float:
    """Weighted average of judge scores, grouped by kind.

    Multiple judges of the same kind (e.g. two LLM judges) split that kind's
    weight evenly — this keeps `weights` expressed per-kind (as
    EVALS_JUDGE_DESIGN.md's suite-level metadata does) rather than needing a
    weight per judge_id, while still letting a suite run more than one judge
    per kind for redundancy.

    Raises ValueError on an empty `results` sequence — there is no
    principled ensemble score for zero judges, and silently returning 0.0
    would be indistinguishable from "every judge scored zero".
    """
    if not results:
        raise ValueError("compute_ensemble_score requires at least one JudgeResult")
    weights = weights or DEFAULT_WEIGHTS

    by_kind: Dict[str, List[JudgeResult]] = {}
    for result in results:
        by_kind.setdefault(result.kind.value, []).append(result)

    total_weight = 0.0
    weighted_sum = 0.0
    for kind, kind_results in by_kind.items():
        kind_weight = weights.get(kind, 0.0)
        if kind_weight <= 0:
            continue
        per_judge_weight = kind_weight / len(kind_results)
        for result in kind_results:
            weighted_sum += result.score * per_judge_weight
            total_weight += per_judge_weight

    if total_weight == 0.0:
        # Every present judge kind has weight 0 (e.g. a suite that only
        # configured a classifier judge but left classifier weight at 0).
        # Falling back to an unweighted mean beats raising here — a
        # misconfigured weight table shouldn't make every scenario unscorable.
        return sum(result.score for result in results) / len(results)
    return weighted_sum / total_weight


@dataclass(frozen=True)
class ReviewDecision:
    """The outcome of applying EVALS_JUDGE_DESIGN.md's routing rules to one
    scenario's ensemble score + judge results.
    """

    verdict: str  # "pass" | "fail" | "human-review"
    reason: str
    ensemble_score: float


def route_for_review(
    results: Sequence[JudgeResult],
    ensemble_score: Optional[float] = None,
    *,
    pass_threshold: float = DEFAULT_PASS_THRESHOLD,
    gray_zone_floor: float = DEFAULT_GRAY_ZONE_FLOOR,
    low_confidence_floor: float = DEFAULT_LOW_CONFIDENCE_FLOOR,
    safety_critical: bool = False,
) -> ReviewDecision:
    """Apply the "Thresholds & actions" + "Human-in-the-loop" sampling rules
    to a scenario's results, in priority order:

      1. Any deterministic-checker failure -> "fail" (deterministic checks
         are exact/high-precision; a failure there is never gray-zone).
      2. safety_critical=True and any judge disagrees with a pass -> forced
         human-review, regardless of score (mandatory review for variance).
      3. Any LLM judge below low_confidence_floor -> "human-review".
      4. ensemble_score in [gray_zone_floor, pass_threshold) -> "human-review".
      5. ensemble_score >= pass_threshold -> "pass", else "fail".
    """
    if ensemble_score is None:
        ensemble_score = compute_ensemble_score(results)

    # Only a STRICT deterministic result is a hard-fail gate: a binary
    # assertion (ExactMatchChecker, RegexChecker) that was actually checked
    # and failed. A graded deterministic score (SimilarityChecker) or an
    # abstention (any checker with no reference to compare against) sets
    # strict=False specifically so it competes on the ensemble threshold
    # below instead of unconditionally failing the scenario — see
    # JudgeResult.strict's docstring for why conflating the two was a bug.
    deterministic_failures = [
        r
        for r in results
        if r.kind is JudgeKind.DETERMINISTIC and r.strict and r.score < 1.0
    ]
    if deterministic_failures:
        names = ", ".join(r.judge_id for r in deterministic_failures)
        return ReviewDecision("fail", f"deterministic checker(s) failed: {names}", ensemble_score)

    if safety_critical:
        threshold = max(pass_threshold, 0.95)
        disagreement = [r for r in results if r.score < threshold]
        if disagreement:
            names = ", ".join(r.judge_id for r in disagreement)
            return ReviewDecision(
                "human-review",
                f"safety-critical scenario: mandatory review triggered by {names}",
                ensemble_score,
            )

    low_confidence = [
        r for r in results if r.kind is JudgeKind.LLM and r.confidence < low_confidence_floor
    ]
    if low_confidence:
        names = ", ".join(r.judge_id for r in low_confidence)
        return ReviewDecision(
            "human-review", f"low-confidence LLM judge(s): {names}", ensemble_score
        )

    if gray_zone_floor <= ensemble_score < pass_threshold:
        return ReviewDecision(
            "human-review",
            f"ensemble score {ensemble_score:.3f} in gray zone [{gray_zone_floor}, {pass_threshold})",
            ensemble_score,
        )

    if ensemble_score >= pass_threshold:
        return ReviewDecision("pass", f"ensemble score {ensemble_score:.3f} >= {pass_threshold}", ensemble_score)

    return ReviewDecision("fail", f"ensemble score {ensemble_score:.3f} < {gray_zone_floor}", ensemble_score)
