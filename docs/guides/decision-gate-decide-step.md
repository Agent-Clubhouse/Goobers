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

- In `shadow` mode the model is called and recorded, but the reported decision
  is always `uncertain`, so the step never changes a run's outcome.
- `uncertain` follows `decisionGate.fallback`: `fail` exits 1, while `agent`
  and `escalate` exit 0 so the workflow can route to an agent or a human.
- When the gate is off the command fails with a clear message. Pass
  `--optional` to report `unavailable` and exit 0 instead, which makes the
  step safe to leave in a shared workflow.
