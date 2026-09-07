# Task and deterministic-stage primitives

Tasks are executable workflow states. A task selects exactly one execution
model with `type`.

## Task type: `deterministic`

Runs a command, inline script, integration, or runner-owned deterministic stage
without invoking a Goober.

```yaml
- name: build
  type: deterministic
  goal: Build the project.
  run:
    command: ["make", "build"]
    workspace: repo
  next: tests
```

Required fields are `name`, `type`, `goal`, and `run`. A deterministic task
must not set `goober`.

### `run` parameters

Exactly one of `command` or `script` is required.

| Field | Type | Description |
| --- | --- | --- |
| `command` | string list | Executable followed by arguments. |
| `script` | string | Inline script interpreted by `sh` on Unix and `cmd.exe` on Windows. |
| `env` | string map | Explicit environment added to the runner's minimal base environment and capability-scoped credentials. |
| `network` | string | DSL 2.0 value `none` denies network access. DSL 3.0 expresses isolation with `runsOn.restrictions`. |
| `workspace` | string | `repo` or `scratch`. A task-level workspace may additionally be `repo-readonly`. |
| `syncBase` | boolean | Merge the freshly fetched base into the run branch before execution; requires writable `repo`. |
| `injectRunContext` | boolean | Supplies `GOOBERS_RUN_ID` and related operational context to a wrapper whose executable is not literally `goobers`. |

Ordinary commands use the implicit `shell` deterministic-stage kind. A command
whose executable is `goobers` may invoke only the admitted built-in commands in
[Stage commands](stage-commands.md).

## Task type: `agentic`

Invokes a named Goober through its configured harness.

```yaml
- name: implement
  type: agentic
  goober: implementer
  goal: Implement the claimed issue and commit the result.
  workspace: repo
  capabilities:
    - repo:push
  policyActions:
    - modify-repository
  next: test
```

Required fields are `name`, `type`, `goal`, and `goober`. An agentic task must
not set `run`.

The task's `capabilities` must be a subset of the Goober's grants. Its
`policyActions` must include every unconditional action prescribed by the
Goober. `onTimeout: salvage` is valid only for an agentic task whose committed
repository diff is a usable partial result.

## Deterministic-stage kind: `shell`

Selected when `inputs.kind` is omitted or set to `shell`.

```yaml
- name: inspect
  type: deterministic
  goal: Inspect the repository.
  workspace: repo-readonly
  run:
    command: ["go", "test", "./internal/workflow"]
  inputs:
    kind: shell
```

The shell kind executes `run.command` or `run.script`. Arbitrary project tools
are allowed. If the command names the Goobers CLI, compile-time admission
restricts its subcommand to the built-in workflow command inventory.

## Deterministic-stage kind: `ci-poll`

Polls provider CI for one pull request. The `run.command` value is a required
placeholder; the runner dispatches by `inputs.kind` and does not shell out.

```yaml
- name: await-ci
  type: deterministic
  goal: Wait for provider CI.
  run:
    command: ["goobers", "ci-poll"]
  inputs:
    kind: ci-poll
    pollTimeoutSeconds: "30m"
  inputsFrom:
    prNumber: open-pr.prNumber
  capabilities:
    - provider:pr:write
  expectedOutputs:
    - ciStatus
    - ciFailedChecks
  next: ci-gate
```

| Input | Required | Default | Description |
| --- | --- | --- | --- |
| `kind` | yes | — | Must be `ci-poll`. |
| `prNumber` | yes | — | Pull-request number or provider identifier. |
| `prOwner` | no | run repository owner | Overrides the repository owner. |
| `prRepo` | no | run repository name | Overrides the repository name. |
| `pollMaxIntervalSeconds` | no | `2m` | Maximum backoff interval, as a Go duration string. |
| `pollTimeoutSeconds` | no | `30m` | Overall poll timeout, bounded below the task's wall-clock limit. |
| `humanPolicyConfigurationIds` | no | all policies gate | Comma-separated provider policy identifiers that require human action and therefore do not drive the fixable-CI branch. |

Required capability: `provider:pr:write`.

The immediately downstream automated gate owns polling cadence through
`automated.pollIntervalSeconds`, expressed as integer seconds. Compilation
injects that value into the `ci-poll` invocation and defaults it to 10 seconds;
a task-level input with the same name is overwritten.

| Output/artifact | Values or meaning |
| --- | --- |
| `ciStatus` | `passing`, `failing`, or `timeout` |
| `ciFailedChecks` | Bounded scalar summary of failing checks |
| `prNumber` | Resolved pull-request identifier |
| `ci-checks.json` | Bounded detailed failure evidence artifact |

Use the [`ci-status`](gates-and-checks.md#ci-status) gate check to branch on the
result.

## Deterministic-stage kind: `external-telemetry`

Runs one query through a configured provider-neutral telemetry connector. The
runner dispatches by `inputs.kind`; `run.command` is a placeholder.

```yaml
- name: query-errors
  type: deterministic
  goal: Query the production error rate.
  workspace: repo-readonly
  run:
    command: ["goobers", "external-telemetry"]
  inputs:
    kind: external-telemetry
    connector: production
    queryRef: queries/error-rate.kql
    shape: point
    window: "30m"
    freshness: "10m"
  capabilities:
    - telemetry:read
  next: error-budget
```

| Input | Required | Description |
| --- | --- | --- |
| `kind` | yes | Must be `external-telemetry`. |
| `connector` | yes | Configured connector name. |
| `query` | exactly one of `query`/`queryRef` | Inline query text. |
| `queryRef` | exactly one of `query`/`queryRef` | Workspace-contained regular file, at most 1 MiB. |
| `parameters` | no | Strict JSON object passed as query parameters. |
| `shape` | no | `point`, `table`, or `time-series`; defaults to `table`. |
| `expectedColumns` | no | Strict JSON array describing the expected result schema. |
| `window` | no | Positive relative Go duration; mutually exclusive with `windowStart`/`windowEnd`. |
| `windowStart` | no | RFC 3339 timestamp. |
| `windowEnd` | no | RFC 3339 timestamp. |
| `freshness` | no | Positive Go duration bounding accepted source-watermark age. |
| `queryTimeout` | no | Positive Go duration narrowing the connector timeout. |
| `queryMaxAttempts` | no | Positive integer narrowing connector attempts. |
| `queryRetryBackoff` | no | Positive Go duration between attempts. |
| `maxRows` | no | Positive result row limit. |
| `maxBytes` | no | Result byte limit of at least 1024 bytes. |

Required capability: `telemetry:read`.

| Output/artifact | Values or meaning |
| --- | --- |
| `dataState` | Normalized connector data state |
| `queryDigest` | Digest of the executed query |
| `telemetryValue` | Scalar value for a one-cell `point` result |
| `external-telemetry-query.json` | Normalized query evidence artifact |

## Fields shared by both task types

| Field | Description |
| --- | --- |
| `inputs` | Static string-valued invocation inputs. |
| `inputsFrom` | Explicit output-to-input bindings from a preceding or qualified source stage. |
| `contextFrom` | Limits rich context pointers to named producer tasks or gates. |
| `capabilities` | Credential grants; see [Capabilities](capabilities.md). |
| `policyActions` | Closed mutation vocabulary; see [Policy actions](policy-actions.md). |
| `minimumIntegrity` | Lowest accepted provenance: `trusted`, `maintainer`, `unapproved`, or `derived`. |
| `retry` | `maxAttempts` including the first, plus optional constant `backoffSeconds`. |
| `timeoutSeconds` | Positive wall-clock limit for one attempt. |
| `limits` | Agent/runtime budgets such as duration, token, and cost limits. |
| `expectedOutputs` | Scalar output or artifact names later states rely on. |
| `continueOnError` | Journals failure but advances to `next`; failed-stage outputs are discarded. |
| `workspace` | Task-level `repo`, `repo-readonly`, or `scratch`. |
| `outbox` | Up to 32 workspace-relative files/directories to export durably. |
| `outboxMirrorPath` | Task override for the local outbox mirror root. |
| `next` | Next task, gate, parallel, or reserved terminal; omission completes successfully. |
| `requiredCapabilities` | DSL 2.0 runner/toolchain tags, not credential grants. |
| `runsOn` | DSL 3.0 placement requirements. |
| `repoFrom` | DSL 3.0 producer stage or stages whose run-branch state this stage consumes. |
| `commitsRepo` | DSL 3.0 declaration that a deterministic command/script advances the run branch. |

The schema remains the exhaustive field-shape contract. This reference
documents the named execution primitives and their composition semantics.
