#!/usr/bin/env python3
"""EvalSuite runner integration — judge harness + adapter wiring (#2667).

Loads an EvalSuite (eval_schema.json's DSL — #2663), executes each
scenario's stages, invokes tool adapters in their configured mode through
the adapter shim (#2666's contract, `evals/adapters/shim.py`), invokes judge
plugins through the judge harness (#2664's contract,
`evals/judge_plugin_interface.py`), and writes a report with a per-judge
score breakdown and artifact links (cassette paths) per scenario.

## Provisional interfaces this builds against

At the time this was written, none of #2663 (DSL/schema), #2664 (judge
harness design), #2665 (sandbox/adapter API + cassette format), or #2666
(adapter shim prototype) had merged to main yet — all four exist as
real, tested work on sibling branches. Rather than block on merge order,
this PR vendors byte-identical copies of the files this runner needs
directly (`evals/eval_schema.json`, `evals/judge_plugin_interface.py`,
`evals/judge_templates/`, `evals/adapters/shim.py`, `evals/adapters/__init__.py`)
from those branches as of this writing. See `evals/README.md` for exactly
which commit each was taken from and how to reconcile once the real PRs
land — the intent is that these paths get overwritten by the real merges,
not that two copies coexist.

Two provisional gaps, called out explicitly rather than silently papered
over:

1. `eval_schema.json`'s `judge` object (as vendored) only has
   `prompt_template`/`threshold` — it does not yet have the `plugins`/
   `weights`/threshold-override fields #2664's design doc asks #2663 to add
   (EVALS_JUDGE_DESIGN.md, "EvalSuite metadata wiring"). This runner reads
   those fields when present (so it's forward-compatible with #2663 adding
   them) but does not require them: a suite with only `prompt_template`/
   `threshold` gets a sane default judge set (see `_default_judges`).
2. No real LLM backend exists in this repo yet. `ProvisionalPromptJudge`
   (below) implements `LLMJudgePlugin._call_model` but does not call a real
   model — it returns a fixed, deliberately low-confidence result so
   `route_for_review`'s low-confidence rule correctly routes it to
   human-review rather than the runner silently pretending to have judged
   something. Swapping in a real model client is future work, not this
   issue's scope (#2664's design doc lists this as explicitly #2667's job
   for the *contract*, not for shipping a production model integration).
"""
from __future__ import annotations

import argparse
import copy
import datetime
import json
import os
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Dict, List, Optional

_EVALS_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(_EVALS_DIR))

from adapters.shim import (  # noqa: E402
    AdapterError,
    AdapterShim,
    CassetteMissingError,
    CassetteStore,
    InvokeResult,
    ShadowRealModeForbiddenError,
)
from judge_plugin_interface import (  # noqa: E402
    DEFAULT_GRAY_ZONE_FLOOR,
    DEFAULT_LOW_CONFIDENCE_FLOOR,
    DEFAULT_PASS_THRESHOLD,
    DEFAULT_WEIGHTS,
    ExactMatchChecker,
    JudgeContext,
    JudgeKind,
    JudgePlugin,
    JudgeRegistry,
    JudgeResult,
    RegexChecker,
    ReviewDecision,
    SimilarityChecker,
    compute_ensemble_score,
    route_for_review,
)

DEFAULT_SCHEMA_PATH = _EVALS_DIR / "eval_schema.json"


class SuiteValidationError(ValueError):
    """Raised when an EvalSuite document doesn't satisfy eval_schema.json's
    required shape. Stdlib-only, structural validation (no jsonschema
    dependency) — mirrors the checks eval_schema.json encodes declaratively,
    kept in sync by `evals/tests/test_runner.py::SchemaAlignmentTests`.
    """


def load_suite(path: str | os.PathLike) -> Dict[str, Any]:
    with open(path, "r", encoding="utf-8") as f:
        suite = json.load(f)
    validate_suite(suite)
    return suite


def validate_suite(suite: Any) -> None:
    if not isinstance(suite, dict):
        raise SuiteValidationError("EvalSuite must be a JSON object")
    if not isinstance(suite.get("suite_name"), str) or not suite["suite_name"]:
        raise SuiteValidationError('EvalSuite must have a non-empty string "suite_name"')
    scenarios = suite.get("scenarios")
    if not isinstance(scenarios, list) or not scenarios:
        raise SuiteValidationError('EvalSuite must have a non-empty "scenarios" array')
    for i, scenario in enumerate(scenarios):
        _validate_scenario(scenario, i)


_VALID_MODES = {"side-by-side", "shadow", "single", "synthetic"}
_VALID_STAGE_TYPES = {"deterministic", "agentic"}
_VALID_ADAPTER_MODES = {"real", "mock", "replay", "no-op"}


def _validate_scenario(scenario: Any, index: int) -> None:
    if not isinstance(scenario, dict):
        raise SuiteValidationError(f"scenario at index {index} must be an object")
    for required in ("id", "name", "input"):
        if required not in scenario:
            raise SuiteValidationError(f"scenario {index} missing required field: {required!r}")
    mode = scenario.get("mode", "single")
    if mode not in _VALID_MODES:
        raise SuiteValidationError(f"scenario {index} has invalid mode: {mode!r}")
    stages = scenario.get("stages", [])
    if not isinstance(stages, list):
        raise SuiteValidationError(f'scenario {index} field "stages" must be an array')
    for j, stage in enumerate(stages):
        _validate_stage(stage, index, j)


def _validate_stage(stage: Any, scenario_index: int, stage_index: int) -> None:
    if not isinstance(stage, dict):
        raise SuiteValidationError(f"stage {stage_index} in scenario {scenario_index} must be an object")
    if "name" not in stage or "type" not in stage:
        raise SuiteValidationError(
            f'stage {stage_index} in scenario {scenario_index} must have "name" and "type"'
        )
    if stage["type"] not in _VALID_STAGE_TYPES:
        raise SuiteValidationError(
            f"stage {stage_index} in scenario {scenario_index} has invalid type: {stage['type']!r}"
        )
    tool_mocks = stage.get("tool_mocks")
    if tool_mocks is not None and not isinstance(tool_mocks, dict):
        raise SuiteValidationError(
            f'stage {stage_index} in scenario {scenario_index} field "tool_mocks" must be an object'
        )


def _adapter_mode(tool_mock_cfg: Dict[str, Any]) -> str:
    """The DSL's field for an adapter's per-stage mode. The sandbox API spec
    (EVALS_SANDBOX_API.md §3.1) names this `mode`; gritty-bear's own sample
    suite (`evals/samples/mvp-evals.json`) uses `mock_type` for the same
    purpose. Accept both, preferring `mode`, so this runner works against
    either convention until #2663/#2665 converge on one.
    """
    mode = tool_mock_cfg.get("mode") or tool_mock_cfg.get("mock_type") or "mock"
    if mode not in _VALID_ADAPTER_MODES:
        raise SuiteValidationError(f"unknown adapter mode: {mode!r}")
    return mode


@dataclass
class StageResult:
    name: str
    type: str
    output: Any
    artifacts: List[Dict[str, Any]] = field(default_factory=list)


@dataclass
class ScenarioRunResult:
    scenario_id: str
    name: str
    mode: str
    baseline_stages: List[StageResult]
    candidate_stages: List[StageResult]
    judge_results: List[JudgeResult]
    ensemble_score: float
    review: ReviewDecision

    def candidate_output(self) -> Any:
        return self.candidate_stages[-1].output if self.candidate_stages else None

    def baseline_output(self) -> Optional[Any]:
        return self.baseline_stages[-1].output if self.baseline_stages else None

    def artifacts(self) -> List[Dict[str, Any]]:
        links: List[Dict[str, Any]] = []
        for stage in self.candidate_stages:
            links.extend(stage.artifacts)
        return links

    def to_report_dict(self) -> Dict[str, Any]:
        return {
            "id": self.scenario_id,
            "name": self.name,
            "mode": self.mode,
            "candidate_output": self.candidate_output(),
            "baseline_output": self.baseline_output(),
            "verdict": self.review.verdict,
            "verdict_reason": self.review.reason,
            "ensemble_score": self.ensemble_score,
            "judges": [
                {
                    "judge_id": r.judge_id,
                    "kind": r.kind.value,
                    "score": r.score,
                    "confidence": r.confidence,
                    "reason": r.reason,
                    "labels": r.labels,
                }
                for r in self.judge_results
            ],
            "artifacts": self.artifacts(),
        }


class ProvisionalPromptJudge(JudgePlugin):
    """A concrete, provisional LLM-kind judge for scenarios that only set
    `judge.prompt_template` (today's `eval_schema.json` shape — no
    `judge.plugins` list). Implements `LLMJudgePlugin`'s `_call_model`
    contract from #2664 without a real model backend: intentionally returns
    a fixed low-confidence result rather than a fabricated score, so
    `route_for_review`'s low-confidence rule (score is never trusted at face
    value) routes every scenario using it to human-review instead of the
    runner silently rubber-stamping a pass/fail nobody actually judged.

    This is not `LLMJudgePlugin` (would require a real `_call_model`
    override to be concrete) — it directly implements `JudgePlugin` and
    documents the prompt it *would* send, so swapping in a real model call
    later is a one-method change, not a redesign.
    """

    kind = JudgeKind.LLM

    def __init__(self, judge_id: str, prompt_template: str) -> None:
        self.judge_id = judge_id
        self.prompt_template = prompt_template

    def evaluate(self, context: JudgeContext) -> JudgeResult:
        rendered = self.prompt_template.format(
            scenario_id=context.scenario_id,
            input=context.input,
            baseline_output=context.baseline_output,
            candidate_output=context.candidate_output,
            expected=context.expected,
            instructions=context.instructions or "",
        )
        return JudgeResult(
            judge_id=self.judge_id,
            kind=self.kind,
            score=0.5,
            reason="no LLM backend wired yet (#2667 provisional stub) — routed for human review, not scored",
            confidence=0.0,
            raw_evidence={"prompt": rendered},
        )


_DETERMINISTIC_PLUGIN_FACTORIES = {
    "exact-match": lambda judge_id, cfg: ExactMatchChecker(judge_id=judge_id),
    "regex-check": lambda judge_id, cfg: RegexChecker(
        judge_id=judge_id,
        required_pattern=cfg.get("required_pattern"),
        forbidden_pattern=cfg.get("forbidden_pattern"),
    ),
    "similarity": lambda judge_id, cfg: SimilarityChecker(judge_id=judge_id),
}


def _build_judge_registry(judge_cfg: Dict[str, Any], *, has_reference: bool = True) -> JudgeRegistry:
    """Builds a JudgeRegistry for one scenario's `judge` config.

    Supports the forward-looking `judge.plugins` list from #2664's
    "EvalSuite metadata wiring" section when present. When absent (today's
    schema), falls back to a default set: an `ExactMatchChecker`
    (deterministic, needs no model) *only when `has_reference` is true* —
    i.e. the scenario has an `expected` value or a baseline to compare
    against — plus a `ProvisionalPromptJudge` if the scenario set
    `prompt_template`.

    `has_reference` matters because `route_for_review`'s rule 1 treats any
    deterministic-kind score below 1.0 as an unconditional scenario failure
    (correctly — a real deterministic mismatch should never be softened into
    "maybe"). A checker with nothing to compare against returns a score that
    means "no reference", not "candidate is wrong"; registering one
    unconditionally would silently hard-fail every reference-less scenario.

    The default is `ExactMatchChecker`, not `SimilarityChecker`, even though
    both are available and both are `kind=DETERMINISTIC`: `SimilarityChecker`
    returns a continuous ratio (e.g. 0.83), and rule 1 fails a scenario on
    *any* deterministic score below 1.0 — so a graded similarity judge in the
    default (implicit) set would hard-fail almost every non-identical
    candidate regardless of the suite's own `judge.threshold`, which is very
    likely not what a suite author expects from an unconfigured default.
    `SimilarityChecker` is still fully available via explicit
    `judge.plugins: ["similarity"]` for suites that want graded similarity
    and understand rule 1's binary-gate semantics going in; see
    `evals/README.md`'s "Provisional/documented gaps" for the full note —
    this is flagged there as worth confirming with #2664, not silently
    routed around.
    """
    registry = JudgeRegistry()
    plugin_specs = judge_cfg.get("plugins")
    if plugin_specs:
        for spec in plugin_specs:
            plugin_id, cfg = (spec, {}) if isinstance(spec, str) else (spec["id"], spec)
            factory = _DETERMINISTIC_PLUGIN_FACTORIES.get(plugin_id)
            if factory is not None:
                registry.register(factory(plugin_id, cfg))
            elif judge_cfg.get("prompt_template"):
                registry.register(ProvisionalPromptJudge(plugin_id, judge_cfg["prompt_template"]))
            else:
                raise SuiteValidationError(
                    f"judge plugin {plugin_id!r} is not a known deterministic checker and no "
                    "prompt_template was provided to build a provisional LLM judge for it"
                )
        return registry

    if has_reference:
        registry.register(ExactMatchChecker(judge_id="exact-match"))
    if judge_cfg.get("prompt_template"):
        registry.register(ProvisionalPromptJudge("prompt-judge", judge_cfg["prompt_template"]))
    return registry


def _judge_weights(judge_cfg: Dict[str, Any]) -> Dict[str, float]:
    weights = judge_cfg.get("weights")
    return dict(weights) if isinstance(weights, dict) else dict(DEFAULT_WEIGHTS)


class Runner:
    """Executes an EvalSuite end-to-end: stages -> adapters -> judges -> report."""

    def __init__(
        self,
        adapter_shim: AdapterShim,
        *,
        default_adapter_mode: str = "mock",
        recorder_mode: Optional[str] = None,
    ) -> None:
        self.adapter_shim = adapter_shim
        self.default_adapter_mode = default_adapter_mode
        # None (the default) means a mode="real" call performs the live call
        # but writes no cassette; "record" is the explicit, separate opt-in
        # EVALS_SANDBOX_API.md/EVALS_CASSETTE.md require for persisting one
        # (see AdapterShim.invoke's docstring in the vendored shim.py).
        self.recorder_mode = recorder_mode

    def run_suite(self, suite: Dict[str, Any]) -> Dict[str, Any]:
        results = [self.run_scenario(scenario) for scenario in suite["scenarios"]]
        return {
            "suite_name": suite["suite_name"],
            "description": suite.get("description"),
            "generated_at": _now_iso(),
            "scenarios": [r.to_report_dict() for r in results],
            "summary": _summarize(results),
        }

    def run_scenario(self, scenario: Dict[str, Any]) -> ScenarioRunResult:
        mode = scenario.get("mode", "single")
        input_obj = scenario.get("input")
        run_baseline = mode == "side-by-side"

        candidate_stages: List[StageResult] = []
        baseline_stages: List[StageResult] = []
        for stage in scenario.get("stages", []):
            candidate_stages.append(
                self._run_stage(scenario, stage, input_obj, variant="candidate", shadow=(mode == "shadow"))
            )
            if run_baseline:
                baseline_stages.append(
                    self._run_stage(scenario, stage, input_obj, variant="baseline", shadow=False)
                )

        judge_cfg = scenario.get("judge") or {}
        # Deliberately `expected`-only, not "expected or baseline present":
        # the default checker is `ExactMatchChecker`, which only ever reads
        # `context.expected` (it has no baseline fallback, unlike
        # `SimilarityChecker`) — a side-by-side scenario with a baseline but
        # no `expected` would register a checker that can never score
        # anything but "no expected value provided", hard-failing via
        # route_for_review's rule 1 for a reason that has nothing to do with
        # the candidate's actual quality. A baseline-only comparison needs a
        # human-considered similarity threshold (`judge.plugins:
        # ["similarity"]`, opted in explicitly), not an implicit default.
        has_reference = scenario.get("expected") is not None
        registry = _build_judge_registry(judge_cfg, has_reference=has_reference)
        context = JudgeContext(
            scenario_id=scenario["id"],
            input=input_obj,
            candidate_output=candidate_stages[-1].output if candidate_stages else None,
            baseline_output=baseline_stages[-1].output if baseline_stages else None,
            expected=scenario.get("expected"),
            instructions=judge_cfg.get("prompt_template"),
        )
        if not registry.all():
            # Nothing to judge this scenario on: no `expected`/baseline for
            # a SimilarityChecker, no `prompt_template` for a provisional
            # LLM stub, and no explicit `judge.plugins`. Route straight to
            # human-review rather than calling compute_ensemble_score (which
            # raises ValueError on an empty result set) or fabricating a
            # judge whose "nothing to compare against" result would trip
            # route_for_review's deterministic-failure rule and silently
            # read as a real failure.
            judge_results: List[JudgeResult] = []
            ensemble_score = 0.0
            review = ReviewDecision(
                verdict="human-review",
                reason="no judge configured for this scenario (no expected/baseline, prompt_template, or judge.plugins)",
                ensemble_score=ensemble_score,
            )
        else:
            judge_results = registry.evaluate_all(context)
            weights = _judge_weights(judge_cfg)
            ensemble_score = compute_ensemble_score(judge_results, weights)
            review = route_for_review(
                judge_results,
                ensemble_score,
                # Today's eval_schema.json (as vendored) names this field
                # `threshold`, not `pass_threshold` — #2664's design doc
                # proposes `pass_threshold` as part of the not-yet-landed
                # extended judge config. Accept both, preferring the
                # extended name so a suite that adopts it later doesn't
                # silently fall back to the default.
                pass_threshold=judge_cfg.get("pass_threshold", judge_cfg.get("threshold", DEFAULT_PASS_THRESHOLD)),
                gray_zone_floor=judge_cfg.get("gray_zone_floor", DEFAULT_GRAY_ZONE_FLOOR),
                low_confidence_floor=judge_cfg.get("low_confidence_floor", DEFAULT_LOW_CONFIDENCE_FLOOR),
                safety_critical=bool(judge_cfg.get("safety_critical", False)),
            )

        return ScenarioRunResult(
            scenario_id=scenario["id"],
            name=scenario.get("name", scenario["id"]),
            mode=mode,
            baseline_stages=baseline_stages,
            candidate_stages=candidate_stages,
            judge_results=judge_results,
            ensemble_score=ensemble_score,
            review=review,
        )

    def _run_stage(
        self,
        scenario: Dict[str, Any],
        stage: Dict[str, Any],
        input_obj: Any,
        *,
        variant: str,
        shadow: bool,
    ) -> StageResult:
        stage_type = stage["type"]
        if stage_type == "deterministic":
            output = _deterministic_output(input_obj, stage.get("seed", 0))
            return StageResult(name=stage["name"], type=stage_type, output=output)

        # agentic: invoke every adapter configured on this stage's tool_mocks.
        artifacts: List[Dict[str, Any]] = []
        outputs: Dict[str, Any] = {}
        tool_mocks = stage.get("tool_mocks") or {}
        for adapter_id, cfg in tool_mocks.items():
            mode = _adapter_mode(cfg)
            # A shadow run's adapters default to no-op for anything with
            # declared side effects unless the scenario explicitly pinned a
            # safer mode — EVALS_SANDBOX_API.md §6.1 rule 2. This runner
            # can't yet read an adapter's side_effects manifest (no adapter
            # registry endpoint exists in the prototype shim), so — as the
            # conservative provisional stand-in — shadow runs force no-op
            # unless the scenario explicitly requested replay/mock, and
            # always refuse real. Documented, not silent.
            if shadow and mode == "real":
                mode = "no-op"
            request = cfg.get("request") or {
                "method": "POST",
                "path": f"/{adapter_id}",
                "headers": {},
                "body": input_obj if isinstance(input_obj, dict) else {"input": input_obj},
            }
            self.adapter_shim.mock_scripts.setdefault(adapter_id, cfg.get("response"))
            try:
                # `shadow` is passed through even though the check above
                # already downgrades a shadow scenario's `real` request to
                # `no-op` before this call — AdapterShim.invoke independently
                # rejects mode="real" when shadow=True
                # (ShadowRealModeForbiddenError), so this is genuine
                # two-layer defense in depth (EVALS_SANDBOX_API.md §6.1 rule
                # 1), not just this runner's own policy layer relying on
                # itself.
                invoke_result: InvokeResult = self.adapter_shim.invoke(
                    adapter_id,
                    mode,
                    request,
                    seed=stage.get("seed", 0),
                    shadow=shadow,
                    scenario_id=scenario["id"],
                    recorder_mode=self.recorder_mode,
                )
            except ShadowRealModeForbiddenError as exc:
                # Per EVALS_SANDBOX_API.md §3.2/§3.3: a policy refusal is
                # "blocked", not "error" — a distinct, expected outcome. This
                # path should be unreachable given the pre-emption above; it
                # stays as a genuine second, independent layer rather than
                # dead code, per the module's shadow-run safety guarantee.
                invoke_result = InvokeResult(
                    status="blocked",
                    mode=mode,
                    response=None,
                    recorded=False,
                    signature="",
                    error={"code": exc.code, "message": str(exc)},
                )
            except CassetteMissingError as exc:
                invoke_result = InvokeResult(
                    status="error",
                    mode=mode,
                    response=None,
                    recorded=False,
                    signature="",
                    error={"code": exc.code, "message": str(exc)},
                )
            except AdapterError as exc:
                invoke_result = InvokeResult(
                    status="error",
                    mode=mode,
                    response=None,
                    recorded=False,
                    signature="",
                    error={"code": exc.code, "message": str(exc)},
                )
            outputs[adapter_id] = invoke_result.response
            artifacts.append(
                {
                    "adapter_id": adapter_id,
                    "mode": mode,
                    "status": invoke_result.status,
                    "signature": invoke_result.signature,
                    "side_effects_performed": invoke_result.side_effects_performed,
                    "cassette_path": self.adapter_shim.store.path_for(adapter_id, invoke_result.signature)
                    if invoke_result.signature
                    else None,
                }
            )

        if not tool_mocks:
            # No adapters configured on this stage — nothing to call; the
            # stage's output is a deterministic placeholder identifying the
            # variant, matching mighty-hare's prototype fallback shape so a
            # suite authored against that prototype still runs here.
            output: Any = f"{variant}:agentic-noop:{stage['name']}"
        elif len(outputs) == 1:
            output = next(iter(outputs.values()))
        else:
            output = outputs

        return StageResult(name=stage["name"], type=stage_type, output=output, artifacts=artifacts)


def _deterministic_output(input_obj: Any, seed: int) -> str:
    import hashlib

    s = json.dumps(input_obj, sort_keys=True)
    h = hashlib.sha256((s + str(seed)).encode()).hexdigest()
    return f"deterministic:{h[:16]}"


def _now_iso() -> str:
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _summarize(results: List[ScenarioRunResult]) -> Dict[str, Any]:
    by_verdict: Dict[str, int] = {}
    for r in results:
        by_verdict[r.review.verdict] = by_verdict.get(r.review.verdict, 0) + 1
    return {"total": len(results), "by_verdict": by_verdict}


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(description="Run an EvalSuite (#2667).")
    parser.add_argument("--suite", required=True, help="Path to an EvalSuite JSON document.")
    parser.add_argument("--out", default="evals/runs", help="Directory to write the report into.")
    parser.add_argument(
        "--cassettes-dir", default=str(_EVALS_DIR / "adapters" / "cassettes"), help="Cassette store root."
    )
    parser.add_argument(
        "--recorder-mode",
        choices=["record"],
        default=None,
        help="Set to 'record' to let a mode=\"real\" call write a new cassette (an explicit, "
        "separate opt-in from mode=\"real\" itself — EVALS_CASSETTE.md §5/§10). "
        "Never set this in CI.",
    )
    args = parser.parse_args(argv)

    suite = load_suite(args.suite)
    shim = AdapterShim(store=CassetteStore(root=args.cassettes_dir), run_id=f"cli-{_now_iso()}")
    runner = Runner(adapter_shim=shim, recorder_mode=args.recorder_mode)
    report = runner.run_suite(suite)

    os.makedirs(args.out, exist_ok=True)
    out_path = os.path.join(args.out, f"{suite['suite_name']}_report.json")
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(report, f, indent=2, sort_keys=True)

    print(f"Wrote report to {out_path}")
    for scenario in report["scenarios"]:
        judge_summary = ", ".join(f"{j['judge_id']}={j['score']:.2f}" for j in scenario["judges"])
        print(f"- {scenario['id']} {scenario['name']}: {scenario['verdict']} ({judge_summary})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
