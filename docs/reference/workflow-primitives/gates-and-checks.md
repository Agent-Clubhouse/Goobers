# Gate evaluator and check primitives

A gate evaluates the immediately preceding state and routes its outcome through
`branches`. A gate declares exactly one evaluator.

```yaml
gates:
  - name: tests-passed
    evaluator: automated
    automated:
      check: status-equals
    branches:
      pass: publish
      fail: "@abort"
```

Every outcome the evaluator can produce must have a branch. The optional
`escalate` control branch handles runner-forced escalation; without it,
escalation terminates at `@escalate`.

## Evaluator: `automated`

Runs one built-in coded check over the preceding result. The runner flattens
these values into the evaluator input:

- `status`: `success`, `failure`, or `blocked`;
- `errorCode`, `errorMessage`, and `errorRetryable`;
- each non-reserved scalar output produced by the preceding stage.

| Field | Required | Description |
| --- | --- | --- |
| `automated.check` | yes | Built-in check name from the sections below. |
| `automated.params` | check-specific | String-valued check parameters. |
| `automated.timeoutSeconds` | no | Positive attempt timeout. |
| `automated.retry.maxAttempts` | no | Total attempts including the first. |
| `automated.retry.backoffSeconds` | no | Constant delay between attempts. |
| `automated.pollIntervalSeconds` | no | Poll cadence for polling checks such as `ci-status`. |

### `status-equals`

Passes when the preceding result's `status` equals `params.equals`.

| Parameter | Required | Default | Values |
| --- | --- | --- | --- |
| `equals` | no | `success` | `success`, `failure`, `blocked` |

**Outcomes:** `pass`, `fail`.

### `failure-class`

Classifies a failed stage so infrastructure failures can be routed separately
from failures in the work.

**Parameters:** none.

**Outcomes:** `pass` for success, `infra` for retryable or recognized
infrastructure failure, and `fail` otherwise.

### `output-equals`

Passes when the named scalar output string equals the configured value.

| Parameter | Required | Description |
| --- | --- | --- |
| `key` | yes | Output key to inspect. |
| `equals` | yes | Expected string value; an explicitly empty string is valid. |

**Outcomes:** `pass`, `fail`.

### `output-not-equals`

Passes when the named scalar output string differs from the configured value.

| Parameter | Required | Description |
| --- | --- | --- |
| `key` | yes | Output key to inspect. |
| `equals` | yes | Disallowed string value; an explicitly empty string is valid. |

**Outcomes:** `pass`, `fail`.

### `output-numeric-gte`

Passes when the named output parses as a number greater than or equal to the
threshold.

| Parameter | Required | Description |
| --- | --- | --- |
| `key` | yes | Output key to inspect. |
| `threshold` | yes | Numeric threshold encoded as a string. |

**Outcomes:** `pass`, `fail`. Missing or non-numeric values fail evaluation.

### `output-numeric-lte`

Passes when the named output parses as a number less than or equal to the
threshold. Parameters and failure behavior match `output-numeric-gte`.

**Outcomes:** `pass`, `fail`.

### `output-numeric-lt`

Passes when the named output parses as a number strictly less than the
threshold. Parameters and failure behavior match `output-numeric-gte`.

**Outcomes:** `pass`, `fail`.

### `output-matches`

Passes when the named scalar output matches an RE2 regular expression.

| Parameter | Required | Description |
| --- | --- | --- |
| `key` | yes | Output key to inspect. |
| `pattern` | yes | Valid RE2 expression. |

**Outcomes:** `pass`, `fail`.

### `ci-status`

Reads the `ciStatus` output emitted by a `ci-poll` stage.

| Parameter | Required | Default | Values |
| --- | --- | --- | --- |
| `equals` | no | `passing` | `passing`, `failing`, `pending` |

**Outcomes:** `pass`, `fail`, `timeout`. A `ciStatus` value of `timeout`
always produces the distinct `timeout` outcome.

### `land-outcome`

Reads the `landOutcome` output emitted by `merge-pr`.

**Parameters:** none.

**Outcomes:** `merged`, `enqueued`, `fail`.

### `queue-outcome`

Reads the `queueOutcome` output emitted by `merge-queue-poll`.

**Parameters:** none.

**Outcomes:** `merged`, `evicted`, `timeout`, `fail`.

## Evaluator: `agentic`

Invokes a reviewer Goober that returns a structured verdict.

| Field | Required | Description |
| --- | --- | --- |
| `agentic.goober` | yes | Reviewer Goober name. |
| `agentic.timeoutSeconds` | no | Positive attempt timeout. |
| `agentic.retry` | no | `maxAttempts` and optional constant `backoffSeconds`. |
| `agentic.workspace` | no | `repo`, `repo-readonly`, or `scratch`; omitted historically means writable `repo`. |
| gate `runsOn` | no, DSL 3.0 | Placement for the reviewer; when present it requires both `cpu` and `memory`. |

**Outcomes:** `pass`, `fail`, `needs-changes`. All three branches are required.
Agentic gates cannot opt into policy actions; use a task for policy-bearing
mutation.

```yaml
- name: review
  evaluator: agentic
  agentic:
    goober: reviewer
    workspace: repo-readonly
  branches:
    pass: publish
    needs-changes: implement
    fail: "@abort"
```

## Evaluator: `human`

Pauses for an explicit decision surfaced through the operator/Portal approval
flow.

| Field | Required | Description |
| --- | --- | --- |
| `human.approvers` | no | Allowed Entra principals or groups. |
| `human.timeoutSeconds` | no | Reserved timeout bound. |
| `human.onTimeout` | no | `remind`, `escalate`, or `reject`. |

Human branch names are the decisions exposed by the workflow. Human gates do
not support `maxRepasses` or `runsOn`, and they cannot appear inside a parallel
branch. Current compilers reject timeout behavior until the target runner
supports it; validate against the target release before declaring
`timeoutSeconds` or `onTimeout`.
