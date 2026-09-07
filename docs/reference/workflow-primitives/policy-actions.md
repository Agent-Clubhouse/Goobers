# Policy-action primitives

`policyActions` are the closed vocabulary of externally mutating decisions a
task or Goober may prescribe. They are separate from capabilities:

- the policy action authorizes the decision;
- the required capability supplies the credential authority to execute it.

```yaml
capabilities:
  - github:pr:merge
policyActions:
  - merge-pr
```

The compiler rejects unknown actions, missing required capabilities, and
built-in commands whose mode prescribes an undeclared action.

## Placement

| Location | Meaning |
| --- | --- |
| `Goober.spec.policyActions` | Actions the persona unconditionally prescribes. |
| `Goober.spec.conditionalPolicyActions` | Actions the persona may prescribe only when the invoking task grants the corresponding capability. |
| `Workflow.spec.tasks[].policyActions` | Actions authorized for this task invocation. |

Agentic tasks must include their Goober's unconditional actions. Agentic gates
cannot opt into policy actions; a review that mutates provider state must hand
off to a task.

## Action reference

| Policy action | Required capability | Authorized decision |
| --- | --- | --- |
| `approve-issue` | `github:issues:approve` | Apply the trusted issue-approval label. |
| `assign-milestone` | `github:milestones:write` | Assign an existing milestone to an issue. |
| `claim-backlog-items` | `github:issues:write` | Claim eligible backlog work. |
| `cancel-pending-ci` | `provider:ci:cancel` | Cancel pending CI for the reviewed commit. |
| `clear-healed-demotions` | `github:pr:write` | Remove PR demotion state that is no longer applicable. |
| `clear-healed-escalations` | `github:pr:write` | Remove PR escalation state that is no longer applicable. |
| `clear-remediation` | `github:issues:write` | Clear remediation routing state after recovery. |
| `close-issue` | `github:issues:write` | Close one issue selected by the workflow. |
| `close-issues` | `github:issues:write` | Close a validated set of issues. |
| `close-pr` | `provider:pr:write` | Close a pull request through the configured provider. |
| `comment-on-issue` | `github:issues:write` | Publish an issue comment. |
| `create-issue` | `github:issues:write` | Create an issue. |
| `delete-branch` | `github:branch:delete` | Delete a remote branch after its lifecycle permits cleanup. |
| `demote-pr` | `github:pr:write` | Remove a PR from landing priority. |
| `edit-issue` | `github:issues:write` | Edit issue content or metadata. |
| `escalate-pr` | `github:pr:write` | Mark a PR for human escalation. |
| `fan-out-remediation` | `github:pr:write` | Create or update remediation routing after merge/reconciliation. |
| `flag-foundation-coupling` | `github:pr:write` | Mark a PR as coupled to foundational work. |
| `flag-scope-drift` | `github:pr:write` | Mark detected overlap or scope drift. |
| `label-issue` | `github:issues:write` | Add or remove ordinary issue labels. |
| `merge-pr` | `github:pr:merge` | Merge a pull request. |
| `modify-repository` | `repo:push` | Make and publish repository changes. |
| `open-or-update-pr` | `provider:pr:write` | Open or update the run pull request. |
| `publish-review` | `github:pr:review` | Publish a provider-native review verdict. |
| `push-repository-branch` | `repo:push` | Push the run's repository branch. |
| `push-pr-branch` | `repo:push` | Force-push or update a selected PR branch. |
| `rebase-pr` | `repo:push` | Rebase and publish a PR branch. |
| `record-merge-refusal` | `github:pr:write` | Persist why an otherwise selected PR was not merged. |
| `record-remediation-checkpoint` | `github:pr:write` | Persist remediation attempt and cause state. |
| `release-backlog-claim` | `github:issues:write` | Release the workflow's backlog claim. |
| `release-pr-claim` | `github:pr:write` | Release a PR-remediation claim. |
| `report-pr-status` | `ado:pr:status` | Publish Goobers evidence as an Azure DevOps PR status. |
| `respond-to-findings` | `github:issues:write` | Publish structured responses to review findings. |
| `resolve-review-threads` | `github:pr:write` | Reply to and resolve remediated review threads. |
| `rework-pr` | `repo:push` | Change and republish a PR's repository content. |
| `route-queue-outcome` | `github:issues:write` | Route an item based on its merge-queue outcome. |
| `route-provider-verdict` | `provider:pr:write` | Apply provider-neutral routing from a review verdict. |
| `route-verdict` | `github:pr:write` | Apply GitHub PR routing from a verdict. |
| `unpark-resolved-siblings` | `github:pr:write` | Unpark sibling PRs whose blocker has resolved. |
| `update-issue` | `github:issues:write` | Update issue state or metadata. |
| `update-pr-branch` | `github:pr:write` | Ask the provider to update a behind-base PR branch. |
| `watch-merge-queue` | `github:pr:merge` | Observe and act on a queued PR until a terminal outcome. |

## Mode-dependent actions

Some command modes change the required action set:

| Command or input | Prescribed actions |
| --- | --- |
| `backlog-query --claim` | `claim-backlog-items`; also `close-issue` for a curation claim |
| `backlog-query --reconcile` | `claim-backlog-items`, `close-issue` |
| `backlog-query --release` | `release-backlog-claim` |
| `backlog-health` feedback mode | `update-issue` |
| `reconcile-branches --delete` or `inputs.deleteBranches: "true"` | `delete-branch` |
| `file-issues` with `inputs.autoApprove: "deterministic-only"` | `approve-issue` in addition to its normal publication actions |
| `file-issues --check` | No mutation actions |
| `respond-to-findings --check` | No mutation actions |

The command's generated [CLI reference](../../cli/README.md) documents the
mode and inputs. `goobers validate` computes the prescribed policy-action set
from the command, flags, and static/dynamic inputs.
