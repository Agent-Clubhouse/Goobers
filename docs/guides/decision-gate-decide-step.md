# Using the decision model in a workflow stage

`goobers decide` lets a deterministic stage ask the instance's decision model
a yes/no question about a JSON document, for example "does this handoff
actually answer the request?". It is opt-in and off unless the instance enables
`decisionGate`.

Use it for semantic judgments only. Whether a JSON document is well formed or
matches a schema is a fact that code decides, not the model.

## Enable it

Set `decisionGate` in the instance config. Endpoint and model are read from
environment variable names, never stored inline. For a Foundry deployment that
has local (key) authentication disabled, use the ambient Azure identity:

```yaml
decisionGate:
  mode: shadow          # off | shadow | enforce
  auth: entra           # omit for a key read from keyEnv
  baseURLEnv: DECISION_BASE_URL
  modelEnv: DECISION_MODEL
  fallback: agent       # agent | escalate | fail
```

The identity needs permission to call the deployment (for Foundry, the
Cognitive Services User role).

## Call it from a stage

```yaml
- name: judge-handoff
  type: deterministic
  goal: Judge whether the handoff answers the request
  run:
    command: [goobers, decide, --name, handoff-complete,
              --question, "Does the summary address every requested item?",
              --state-file, handoff.json, --accept, "0.9", --reject, "0.1"]
  inputs:
    resultFile: decide.json
```

The result file contains `decision` (`yes`, `no`, `uncertain` or
`unavailable`), `probability`, `mode`, `fallback` and `stateDigest`.

The optional `--true` and `--false` flags describe what each answer means.
Either flag can be supplied independently; omitted, empty, or whitespace-only
values are not sent as criteria. With neither flag, the question's instructions
alone define the decision.

- In `shadow` mode the model is called and recorded, but the reported decision
  is always `uncertain`, so the step never changes a run's outcome.
- `uncertain` follows `decisionGate.fallback`: `fail` exits 1, while `agent`
  and `escalate` exit 0 so the workflow can route to an agent or a human.
- When the gate is off the command fails with a clear message. Pass
  `--optional` to report `unavailable` and exit 0 instead, which makes the
  step safe to leave in a shared workflow.

## PR description vs diff (advisory)

`decisiongate.Gate.EvaluateDescription` scores whether a pull request title
and body agree with its diff. Code computes the facts (changed files, line
counts and a size bucket) and skips the model for an empty title or diff. The
model judges one yes/no question behind the `description_matches_diff`
threshold. The result is `match`, `mismatch` or `unsure`; `unsure` and any error
leave publishing untouched. It is advisory: a caller may add a review note on
`mismatch` and must never block on it. The result carries the size bucket and
probability, not the text, so shadow scores can be compared across diff sizes.

## Review comment severity (advisory)

`decisiongate.Gate.EvaluateSeverity` labels a review comment `blocking`,
`nit`, `question` or `unsure`. Code decides first: an explicit `nit:`,
`question:` or `blocking:` prefix, or a short comment that is only a question,
never reaches the model. Otherwise the model judges one yes/no question (must
this be fixed before merge) behind the `review_comment_blocking` threshold;
`uncertain` and any error return `unsure`, and the caller keeps its existing
behavior of treating the comment as actionable.

`ShadowSeverity` and `SeverityTally` record the model's label next to a human
or reviewer-agent label and report agreement only above a minimum sample.
Nothing is enforced until those measured counts justify it.

## CI failure triage (advisory)

Today every failed CI run goes to the implementer, flakes and infrastructure
faults included. `decisiongate.Gate.EvaluateCIFailure` classifies a failed CI
log as `flaky`, `real`, `infra` or `environment`, behind the
`ci_failure_class` threshold (default accept 0.85). It is off unless
`decisionGate` is on, and a caller must treat it as advice only.

- **Code decides facts first.** A shared baseline failure
  (`--ci-baseline shared-baseline-failure`, from `internal/baseline`) is
  `environment`; known infrastructure signatures (runner loss, connection
  resets, 502/503/504, rate limits, full disks) are `infra`; missing-tool
  errors are `environment`. None of these reach the model.
- **The model sees a bounded, redacted excerpt.** The log is stripped of ANSI
  codes, credentials, URL userinfo, emails and home paths, and only then cut
  to 8 KiB (the start and the end of the log). The model picks one class; a
  probability below the threshold, an unknown option or any error is `unsure`,
  and `unsure` means the existing path runs.
- **Shadow first.** In `shadow` mode `goobers decide --ci-log <file>` still
  records its prediction but always reports `class: unsure`, so a run's
  routing never changes.

```yaml
- name: triage-ci
  type: deterministic
  goal: Advise on a failed CI log
  run:
    command: [goobers, decide, --optional, --ci-log, ci.log,
              --run-key, <run-id>]
```

### Measuring it

With `--run-key`, each prediction is appended to
`scheduler/ci-failure-triage/ledger.jsonl` in the instance root: digests and
numbers, never log text. When the eventual cause is known (a later pass on the
same head, a human label), record it:

```sh
goobers decide --ci-outcome real --run-key <id>
goobers decide --ci-matrix
```

`--ci-matrix` joins each prediction to its outcome (the latest line for a run
wins) and reports counts of predicted against eventual class, kept apart for
model-decided and code-decided predictions, plus `pending` (no outcome yet),
`orphans`, `unsure` and `wronglySkipped` (the model said flaky, infra or
environment and the cause was real, the costly miss). `modelAccuracy` appears
only once `modelJudged` reaches `minSample` (30). Graduating from shadow to
enforce should wait for those counts, not intuition.
