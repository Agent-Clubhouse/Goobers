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

## Let an agent stage call it

Agent stages can ask the same question through a `decide` MCP tool. It is
denied by default: a task sees the tool only when it is listed under
`decisionGate.agentTool.tasks`, and only while the gate is `shadow` or
`enforce`.

```yaml
decisionGate:
  mode: shadow
  auth: entra
  baseURLEnv: DECISION_BASE_URL
  modelEnv: DECISION_MODEL
  fallback: agent
  agentTool:
    tasks: [review-handoff]   # specific task ids; no wildcards
    maxCalls: 5               # model calls per stage invocation (max 50)
  gate:
    thresholds:
      handoff-complete: {accept: 0.9, reject: 0.1}
```

- The tool takes `name`, `question`, `state` and optional `true`/`false`
  text. `name` must be a threshold configured under `gate.thresholds`, so the
  instance controls which questions exist and where their cut-offs sit.
- The agent never receives the endpoint or credential. The tool runs in the
  runner's MCP server and resolves them from that process's environment or
  ambient identity.
- Calls past `maxCalls` are refused. With `gate.cacheEntries` set, identical
  questions about identical state reuse the digest cache and are not re-sent
  (they still count against the budget).
- The result is `decision`, `probability`, `mode`, `stateDigest`,
  `callsRemaining` and, when uncertain, `fallback`. In `shadow` mode the
  decision is always `uncertain`.
- This applies to the local runner. Worker pods do not receive the grant.

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

## Stall and loop detection (advisory, shadow)

Set `decisionGate.stallDetection: true` with `mode: shadow` to score running
agentic stages. The harness already checkpoints a stage's transcript about once
a minute; each checkpoint now also hands a bounded (32 KiB), redacted tail of
that stream to the observer, so there is no per-turn cost. The tail is redacted
as a whole, so a secret split across two checkpoints is still caught before any
model call.

Code decides facts first: a short line cycle repeating back to back with no
file-change line in the window is a `stalled` verdict with basis `code` and no
model call. Otherwise a window with enough lines goes to the model, which judges
one yes/no question (no meaningful progress) behind the `transcript_no_progress`
threshold, at most eight times per stream. `unsure` and any error change
nothing. Bursts are coalesced to the latest window per stream, so a fast loop is
never lost behind earlier snapshots.

When the stage ends the harness labels the stream with whether it hit its time
budget, and the log carries `decisiongate.stall-shadow` (per window) and
`decisiongate.stall-outcome` (flagged window vs budget hit). `StallTally` turns
outcomes into flagged-and-hit, flagged-and-completed and missed counts, and
`Precision` refuses a figure below a minimum sample. Nothing is enforced until
those counts justify it. Only the local runner is wired; pod stages are not.
