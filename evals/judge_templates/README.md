# Judge prompt templates

Sample LLM judge prompts for EvalSuite (#2664). Each `.txt` file is a
`str.format()` template consumed by `LLMJudgePlugin.render_prompt()` in
`evals/judge_plugin_interface.py` — the placeholders below (`{input}`,
`{candidate_output}`, etc.) are exactly `JudgeContext`'s fields.

| Template | Judge kind | Used for |
|---|---|---|
| `general_quality.txt` | Single-candidate quality | Clarity/correctness/completeness scoring against `expected`, no baseline needed. Default for `mode: single` scenarios. |
| `side_by_side_comparison.txt` | Baseline vs. candidate | A/B scenarios (`mode: side-by-side`) — judges whether the candidate is at least as good as the baseline, not just "good in isolation". |
| `safety_no_side_effects.txt` | Safety / shadow-run | Safety-critical and `mode: shadow` scenarios — the question is "was this action safe to have taken", independent of task quality. |

## Response contract

Every template instructs the model to return JSON matching
`LLMJudgePlugin`'s expected response shape:

```json
{
  "score": 0.0,
  "labels": ["string", "..."],
  "reason": "string",
  "confidence": 0.0
}
```

`score` and `confidence` must be in `[0.0, 1.0]` — `JudgeResult.__post_init__`
raises if a `_call_model` implementation returns anything outside that range,
so a misbehaving model surfaces as a loud error rather than a silently wrong
ensemble score.

## Adding a new template

1. Write the `.txt` file here. Keep the JSON-response instruction verbatim
   from an existing template — the parsing contract in `LLMJudgePlugin`
   depends on it.
2. Add a row to the table above.
3. Reference it from an EvalSuite's `scenarios[].judge.prompt_template`
   field (see `sample_evalsuite.json` at the repo root for the DSL), or
   wire it into a concrete `LLMJudgePlugin` subclass once #2667 exists.
