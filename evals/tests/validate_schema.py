"""Schema validation tests for the EvalSuite DSL (#2663).

Covers: the schema is itself well-formed, every checked-in sample suite
validates against it, and a set of deliberately malformed documents are
rejected so the runner fails fast on bad input rather than misbehaving
downstream.
"""

import copy
import glob
import json
import os

import pytest
from jsonschema import Draft7Validator

EVALS_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SCHEMA_PATH = os.path.join(EVALS_ROOT, "eval_schema.json")
SAMPLES_DIR = os.path.join(EVALS_ROOT, "samples")


def load_json(path):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def load_schema():
    return load_json(SCHEMA_PATH)


def sample_paths():
    return sorted(glob.glob(os.path.join(SAMPLES_DIR, "*.json")))


def minimal_valid_suite():
    """The smallest document that satisfies the schema, for building
    deliberately-broken variants without hand-writing them from scratch."""
    return {
        "suite_name": "minimal",
        "scenarios": [{"id": "s1", "name": "s1", "input": "hello"}],
    }


def test_schema_is_valid_json_schema():
    schema = load_schema()
    Draft7Validator.check_schema(schema)


def test_schema_declares_required_top_level_fields():
    schema = load_schema()
    assert schema.get("required") == ["suite_name", "scenarios"]


@pytest.mark.parametrize("path", sample_paths(), ids=os.path.basename)
def test_sample_suite_matches_schema(path):
    schema = load_schema()
    sample = load_json(path)
    validator = Draft7Validator(schema)
    errors = list(validator.iter_errors(sample))
    if errors:
        msgs = "\n".join(str(e) for e in errors)
        pytest.fail(f"{os.path.basename(path)} failed schema validation:\n{msgs}")


def test_at_least_one_sample_suite_is_checked_in():
    assert sample_paths(), f"No sample suites found under {SAMPLES_DIR}"


@pytest.mark.parametrize(
    "path",
    sample_paths(),
    ids=os.path.basename,
)
def test_sample_suite_scenario_ids_are_unique(path):
    sample = load_json(path)
    ids = [scenario["id"] for scenario in sample.get("scenarios", [])]
    duplicates = {i for i in ids if ids.count(i) > 1}
    assert not duplicates, f"{os.path.basename(path)} has duplicate scenario ids: {duplicates}"


def assert_invalid(document):
    schema = load_schema()
    validator = Draft7Validator(schema)
    errors = list(validator.iter_errors(document))
    assert errors, f"Expected schema validation to reject: {document!r}"


def test_missing_scenarios_is_rejected():
    assert_invalid({"suite_name": "no-scenarios"})


def test_missing_suite_name_is_rejected():
    assert_invalid({"scenarios": [{"id": "s1", "name": "s1", "input": "hi"}]})


def test_scenario_missing_input_is_rejected():
    suite = minimal_valid_suite()
    del suite["scenarios"][0]["input"]
    assert_invalid(suite)


def test_scenario_missing_id_is_rejected():
    suite = minimal_valid_suite()
    del suite["scenarios"][0]["id"]
    assert_invalid(suite)


def test_empty_scenarios_array_is_rejected():
    suite = minimal_valid_suite()
    suite["scenarios"] = []
    assert_invalid(suite)


def test_unknown_top_level_property_is_rejected():
    suite = minimal_valid_suite()
    suite["unexpected_field"] = "surprise"
    assert_invalid(suite)


def test_invalid_scenario_mode_is_rejected():
    suite = minimal_valid_suite()
    suite["scenarios"][0]["mode"] = "not-a-real-mode"
    assert_invalid(suite)


def test_invalid_stage_type_is_rejected():
    suite = minimal_valid_suite()
    suite["scenarios"][0]["stages"] = [{"name": "step", "type": "not-a-real-type"}]
    assert_invalid(suite)


def test_stage_missing_type_is_rejected():
    suite = minimal_valid_suite()
    suite["scenarios"][0]["stages"] = [{"name": "step"}]
    assert_invalid(suite)


def test_tool_mocks_valid_adapter_mode_is_accepted():
    # stages[].tool_mocks.<adapter_id>.mode, per EVALS_SANDBOX_API.md's
    # real/mock/replay/no-op contract (#2671/#2672 coordination).
    suite = minimal_valid_suite()
    suite["scenarios"][0]["stages"] = [
        {
            "name": "step",
            "type": "agentic",
            "tool_mocks": {"bank_api": {"mode": "no-op", "response": {"status": "ok"}}},
        }
    ]
    schema = load_schema()
    validator = Draft7Validator(schema)
    errors = list(validator.iter_errors(suite))
    assert not errors, f"Expected valid tool_mocks.mode to pass: {errors}"


def test_tool_mocks_invalid_adapter_mode_is_rejected():
    suite = minimal_valid_suite()
    suite["scenarios"][0]["stages"] = [
        {
            "name": "step",
            "type": "agentic",
            "tool_mocks": {"bank_api": {"mode": "not-a-real-mode"}},
        }
    ]
    assert_invalid(suite)


@pytest.mark.parametrize("threshold", [-0.1, 1.1])
def test_judge_threshold_out_of_range_is_rejected(threshold):
    suite = minimal_valid_suite()
    suite["scenarios"][0]["judge"] = {"prompt_template": "ok?", "threshold": threshold}
    assert_invalid(suite)


def test_valid_document_is_not_mutated_by_validation():
    # Guards against a test bug where a shared fixture is mutated in place
    # by one of the "malformed variant" tests above, silently poisoning a
    # later test in the same run.
    suite = minimal_valid_suite()
    snapshot = copy.deepcopy(suite)
    validator = Draft7Validator(load_schema())
    list(validator.iter_errors(suite))
    assert suite == snapshot
