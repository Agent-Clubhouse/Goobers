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

A git command that exits 128 because a revision it was given does not exist
in the checkout (`fatal: ambiguous argument '...': unknown revision or path not
in the working tree`, or `fatal: bad revision '...'`) is recognized
infrastructure: it fails the same way for every change, so it routes to `infra`
instead of spending an implementation repass. Local run workcopies are linked
worktrees of a mirror that stores the project's branches as `refs/heads/*`
without `refs/remotes/origin/*`, so name a base as `main...HEAD`, not
`origin/main...HEAD`.

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

#### Checks that finished without a verdict (#6817)

A check that ended without judging the code is never `failing` and never
`passing`. These are a GitHub check or workflow run concluded `cancelled`,
`startup_failure` or `stale`, or an Azure DevOps build policy rejected for a
build whose result is `canceled`. Providers report such a check as pending
with `noVerdict` set. A CI-provider outage that cancels jobs before they run
therefore never routes to CI remediation, and no merge gate accepts it. On
the GitHub `actions/runs` fallback a cancelled workflow run still fails when
one of its jobs failed.

Nothing reruns such a check by itself, so `ci-poll` does not wait out its
timeout for it. Instead it acts once two consecutive polls of the same head
find every non-passing check finished without a verdict:

1. If the stage sets `retryFailedChecksMaxAttempts`, `ci-poll` reruns the
   checks, up to that many times.
2. Once those reruns are spent or refused, or if none were configured, the
   stage's `noVerdict` input decides what happens:
   - `fail` (the default): the stage fails with the infrastructure failure
     code `ci_no_verdict`. The runner retries the stage within its
     infrastructure budget. A run that ends on `ci_no_verdict` is an
     infrastructure fault, not a work failure, so it does not count toward
     the failure-streak park.
   - `continue`: the stage reports `ciStatus: pending` as a determined
     result. The run goes on, and the no-verdict checks are recorded in
     `ci-checks.json`.

The bound differs by lane:

- **implementation** (`ci-poll`, no reruns, `noVerdict: fail`). The run ends
  after the stage's attempts, which are its `retry.maxAttempts` plus one
  infrastructure retry, at two polls each. The open PR is left as it is, not
  parked, and `backlog-query` does not reselect its issue while the PR is
  open.
- **pr-remediation** (`retry-failed-ci`, one rerun, `noVerdict: continue`).
  - A PR whose only problem is no-verdict CI is never selected.
  - A PR selected for another cause reruns its CI once, then goes on to
    `remediation-checkpoint`. That cause is fixed and charged as usual, and
    the agent sees the no-verdict checks as context.
- **merge-review.** `merge-pr` refuses with a `ci-no-verdict:` reason.
  `record-merge-refusal` does not count that reason toward demotion.
  Instead it posts one sticky comment per head asking for a CI rerun. The
  PR merges once a rerun turns CI green.

`startup_failure` is a trade-off. GitHub also reports it for a workflow
definition error, which a pull request that edits `.github/workflows/` can
cause. Such a PR is no longer remediated for failing CI. It waits for a
person: the merge-review comment asks for one. When the PR is remediated for
another cause, the startup failure that survived its rerun reaches the agent
in `ci-checks.json`, where it can be recognized as the PR's own error.

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
