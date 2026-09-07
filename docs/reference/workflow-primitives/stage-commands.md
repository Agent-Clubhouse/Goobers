# Built-in stage command primitives

A deterministic shell task invokes a built-in command with:

```yaml
- name: query
  type: deterministic
  goal: Claim one eligible item.
  run:
    command: ["goobers", "backlog-query", "--claim"]
  inputs:
    trustLabel: "goobers:approved"
    resultFile: "claimed-item.json"
```

The command's flags select its mode. Task `inputs` and `inputsFrom` populate the
stage invocation envelope. The generated CLI page linked for each command is
the canonical behavior and parameter reference; it documents required inputs,
outputs, modes, and exit behavior together with command-line flags.

The compiler admits only the following `goobers` subcommands in a shell stage.
Operator commands such as `up`, `status`, and `init` are intentionally absent.

| Command | Purpose and parameter reference |
| --- | --- |
| `__demo-provider` | Internal provider used only by the credential-free workflow seeded by `goobers init --demo`; not a general authoring surface. |
| [`apply-verdict`](../../cli/README.md#goobers-apply-verdict) | Publish a managed or advisory merge-review verdict. |
| [`backlog-assignment`](../../cli/README.md#goobers-backlog-assignment) | Assign eligible backlog items from a configured roster. |
| [`backlog-dedupe`](../../cli/README.md#goobers-backlog-dedupe) | Surface ranked duplicate candidates for curator judgment. |
| [`backlog-health`](../../cli/README.md#goobers-backlog-health) | Snapshot ready-pool depth and age. |
| [`backlog-query`](../../cli/README.md#goobers-backlog-query) | Query, claim, reconcile, or release backlog items. |
| [`cancel-pending-ci`](../../cli/README.md#goobers-cancel-pending-ci) | Cancel pending provider CI for an exact reviewed PR head. |
| [`check-fail-first`](../../cli/README.md#goobers-check-fail-first) | Enforce fail-first evidence for a new workflow gate. |
| [`check-issue-staleness`](../../cli/README.md#goobers-check-issue-staleness) | Route a PR when its originating issue changed after implementation began. |
| [`docs-churn`](../../cli/README.md#goobers-docs-churn) | Emit the documentation-drift churn digest. |
| [`elect-lander`](../../cli/README.md#goobers-elect-lander) | Elect the landing PR among a merge-review cohort. |
| [`file-issues`](../../cli/README.md#goobers-file-issues) | File a validated nomination batch as deduplicated, budgeted issues. |
| [`gate-removal-guard`](../../cli/README.md#goobers-gate-removal-guard) | Block a Tutor run that weakens its own flagged gate without proof. |
| [`gather-ci-failures`](../../cli/README.md#goobers-gather-ci-failures) | Add failing CI diagnostics to a remediation brief. |
| [`gather-implement-context`](../../cli/README.md#goobers-gather-implement-context) | Load first-pass implementation review and hot-file context. |
| [`gather-issue-context`](../../cli/README.md#goobers-gather-issue-context) | Add originating issue bodies to a remediation brief. |
| [`gather-pr-context`](../../cli/README.md#goobers-gather-pr-context) | Select a remediation PR and load its context. |
| [`gather-review-threads`](../../cli/README.md#goobers-gather-review-threads) | Add native reviews and anchored threads to a remediation brief. |
| [`gather-sibling-context`](../../cli/README.md#goobers-gather-sibling-context) | Load other open PRs as review evidence. |
| [`ios-simulator-test`](../../cli/README.md#goobers-ios-simulator-test) | Run XCUITest and parse its xcresult diagnostics. |
| [`issue-close-out`](../../cli/README.md#goobers-issue-close-out) | Comment on and close or park a claimed issue. |
| [`mcp-io`](../../cli/README.md#goobers-mcp-io) | MCP process auto-wired by the harness for artifact publication and reading; authors normally select it through artifact declarations rather than a task. |
| [`merge-pr`](../../cli/README.md#goobers-merge-pr) | Evaluate merge conjuncts and land directly or through a merge queue. |
| [`merge-queue-poll`](../../cli/README.md#goobers-merge-queue-poll) | Watch an enqueued PR to a terminal queue outcome. |
| [`open-pr`](../../cli/README.md#goobers-open-pr) | Open or update the run's pull request. |
| [`post-merge`](../../cli/README.md#goobers-post-merge) | Perform post-merge fan-out and close referenced issues. |
| [`pr-claim`](../../cli/README.md#goobers-pr-claim) | Check PR liveness or release its remediation claim. |
| [`pr-comment-watch`](../../cli/README.md#goobers-pr-comment-watch) | Find managed PRs with unresolved human comments. |
| [`pr-select`](../../cli/README.md#goobers-pr-select) | Select one managed or advisory PR for merge review. |
| [`preflight-repo-write`](../../cli/README.md#goobers-preflight-repo-write) | Verify that the configured credential may push the run's branch namespace without mutation. |
| [`publish-batch`](../../cli/README.md#goobers-publish-batch) | Publish a verified decomposition batch behind one eligibility barrier. |
| [`push-branch`](../../cli/README.md#goobers-push-branch) | Push the worktree's checked-out run branch. |
| [`push-remediated`](../../cli/README.md#goobers-push-remediated) | Force-push a remediated branch and clear remediation state. |
| [`rebase-pr`](../../cli/README.md#goobers-rebase-pr) | Rebase and route finding-driven remediation. |
| [`reconcile-branches`](../../cli/README.md#goobers-reconcile-branches) | Report or delete bounded stale Goobers branch candidates. |
| [`reconcile-post-merge`](../../cli/README.md#goobers-reconcile-post-merge) | Reconcile pull requests that merged after their watching run ended. |
| [`record-merge-refusal`](../../cli/README.md#goobers-record-merge-refusal) | Record a merge refusal and demote a persistently stuck lander. |
| [`remediation-checkpoint`](../../cli/README.md#goobers-remediation-checkpoint) | Enforce durable per-cause remediation attempt budgets. |
| [`report-pr-status`](../../cli/README.md#goobers-report-pr-status) | Publish verdict and CI evidence as a provider-native PR status. |
| [`resolve-review-threads`](../../cli/README.md#goobers-resolve-review-threads) | Reply to and resolve remediated native review threads. |
| [`respond-to-findings`](../../cli/README.md#goobers-respond-to-findings) | Post validated per-finding remediation responses. |
| [`select-source`](../../cli/README.md#goobers-select-source) | Select and claim an unconsumed decomposition disposition. |
| [`self-update`](../../cli/README.md#goobers-self-update) | Stage and request a supervised Goobers binary update. |
| [`set-milestone`](../../cli/README.md#goobers-set-milestone) | Assign an existing milestone to an issue. |
| [`telemetry-query`](../../cli/README.md#goobers-telemetry-query) | Emit versioned candidate findings from local telemetry. |
| [`update-behind-pr`](../../cli/README.md#goobers-update-behind-pr) | Update a clean behind-base PR through the provider API or route it to remediation. |
| [`validate`](../../cli/README.md#goobers-validate) | Validate an instance or checked-in configuration tree; used by Tutor workflows as a deterministic validation stage. |
| [`validate-plan`](../../cli/README.md#goobers-validate-plan) | Validate a decomposition plan against its selector artifact and live parent. |

## Kind-dispatched placeholder commands

`ci-poll` and `external-telemetry` intentionally do not appear in the shell
command inventory. A task writes a placeholder command for schema consistency
and selects the actual runner implementation with `inputs.kind`:

```yaml
run:
  command: ["goobers", "ci-poll"]
inputs:
  kind: ci-poll
```

Their complete input and output contracts are documented in
[Tasks and stages](tasks-and-stages.md#deterministic-stage-kind-ci-poll).

## Capability and policy-action admission

A built-in command may require:

- credential `capabilities`, derived from the provider-stage manifest; and
- explicit `policyActions` for externally mutating decisions.

Flags and inputs can change that contract. For example,
`backlog-query --claim` prescribes `claim-backlog-items`, while
`backlog-query --read-only` does not. Always copy the mode-specific grants from
the command reference or a shipped workflow and run `goobers validate`; do not
grant a command every capability its broader command family might use.
