# Credential capability primitives

`capabilities` declare authority made available to a task. They do not select
where the task runs and do not by themselves authorize a policy decision.

```yaml
- name: publish
  type: deterministic
  goal: Open or update the run pull request.
  run:
    command: ["goobers", "open-pr"]
  capabilities:
    - provider:pr:write
  policyActions:
    - open-or-update-pr
```

Use the narrowest grant accepted by the selected stage. An agentic task's
capabilities must also appear on its referenced Goober.

## Stage-declarable capabilities

| Capability | Authority |
| --- | --- |
| `repo:read` | Read-only checkout of the target repository for the stage. |
| `repo:push` | Commit/push authority for the run branch. |
| `github:issues:read` | Read GitHub issues without mutation. |
| `github:issues:write` | Query, create, edit, label, comment on, or close GitHub issues, excluding the trusted approval label. |
| `github:milestones:write` | Assign an existing GitHub milestone. |
| `github:issues:approve` | Apply the trusted `goobers:approved` label. |
| `provider:pr:write` | Provider-neutral pull-request create/update operations. |
| `github:pr:write` | GitHub-specific pull-request mutation. |
| `github:pr:review` | Submit provider-native GitHub approve/request-changes reviews. |
| `provider:ci:cancel` | Cancel pending provider CI for a pinned commit. |
| `github:branch:delete` | Delete a remote GitHub branch ref. |
| `github:pr:merge` | Merge a GitHub pull request. |
| `contents:read` | Fetch a separately declared read-only reference repository. |
| `ado:code:read` | Read Azure Repos code and pull requests. |
| `ado:pr:comment` | Post Azure Repos PR threads without vote or completion authority. |
| `ado:pr:write` | Open or update Azure Repos pull requests. |
| `ado:pr:status` | Publish Azure Repos pull-request statuses. |
| `ado:pr:complete` | Complete an Azure Repos pull request. |
| `ado:work-items:write` | Update explicitly selected Azure Boards work items. |
| `telemetry:read` | Read local telemetry and configured external telemetry connectors. |
| `journal:read` | Resolve evidence from another run's journal. |
| `agent:model` | Supply an agentic harness with its model credential. |

## Runner-only capability

| Capability | Authority |
| --- | --- |
| `configrepo:read` | Lets the daemon read its workflow-configuration repository. Tasks and Goobers cannot declare it. |

The runner derives repository-qualified keys such as
`contents:read@owner/name` internally when materializing additional
repositories. Those keys are not canonical stage capabilities and must not
appear in task or Goober YAML.

## Placement

| Location | Meaning |
| --- | --- |
| `Goober.spec.capabilities` | Maximum credential authority available to that persona. |
| `Workflow.spec.tasks[].capabilities` | Authority granted to this invocation. For an agentic task, it must be a subset of the Goober grants. |

Do not confuse credential capabilities with these similarly named fields:

| Field | Vocabulary |
| --- | --- |
| DSL 2.0 task `requiredCapabilities` | Open runner/toolchain tags such as `xcode` or `dotnet@8`. |
| DSL 3.0 `runsOn.capabilities` | Open runner/toolchain placement tags. |
| `Workflow.spec.requires.capabilities` | Provider contract features such as `pr.merge`; explicit values replace derived requirements. |

## Capability and policy-action relationship

A capability says that a stage **can** perform an operation. A
[`policyAction`](policy-actions.md) says that the workflow or persona is
authorized to choose that externally visible mutation. Policy-bearing stages
must declare both.

The only non-reflexive capability subsumption currently recognized is
`github:issues:write` satisfying a `github:issues:read` requirement. Prefer the
narrow read grant when the task does not mutate issues.
