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
| `github:pr:merge` | Merge a pull request. The landing authority on every provider: on Azure DevOps it completes the pull request unless the stage also declares `ado:pr:complete`. |
| `contents:read` | Fetch a separately declared read-only reference repository. |
| `ado:code:read` | Read Azure Repos code and pull requests. Accepted but inert in DSL 2.0 (warns `CAP006`): repository reads need no capability. See [Azure DevOps](#azure-devops-and-the-dsl-20-rebinding-rule). |
| `ado:pr:comment` | Post Azure Repos PR threads without vote or completion authority. Accepted but inert in DSL 2.0 (warns `CAP006`): declare `github:pr:write`. |
| `ado:pr:write` | Open or update Azure Repos pull requests. Accepted but inert in DSL 2.0 (warns `CAP006`): declare `github:pr:write`. |
| `ado:pr:status` | Publish Azure Repos pull-request statuses. Optional on the `report-pr-status` policy action, which requires `github:pr:write`; declaring `ado:pr:status` alongside it is accepted but not required. |
| `ado:pr:complete` | Complete an Azure Repos pull request. Optional: accepted on `merge-pr` and `merge-queue-poll` alongside the required `github:pr:merge`, and when declared, completion uses its credential instead. |
| `ado:work-items:write` | Update explicitly selected Azure Boards work items. Consumed only by `open-pr`, which links an Azure DevOps pull request to its work item; elsewhere it is inert in DSL 2.0 (warns `CAP006`): declare `github:issues:write`. |
| `telemetry:read` | Read local telemetry and configured external telemetry connectors. |
| `journal:read` | Resolve evidence from another run's journal. |
| `agent:model` | Supply an agentic harness with its model credential. |

## Azure DevOps and the DSL 2.0 rebinding rule

DSL 2.0 capability names were written GitHub-first, and DSL 2.0 is frozen, so
it takes no new names. Instead, a `github:*` capability declared on a stage
whose command dispatches through the provider seam authorizes **the same
operation on the provider the stage routes to**, and selects that provider's
credential. Gitea has always worked this way, and Azure DevOps now does too.
This adds no vocabulary and changes nothing on GitHub.

| Capability | Routes to | Azure DevOps operation |
| --- | --- | --- |
| `github:issues:read`, `github:issues:write`, `github:issues:approve`, `github:milestones:write` | the gaggle's backlog provider | Boards work items, tags, comments, state |
| `github:pr:write`, `github:pr:review`, `github:branch:delete` | the gaggle's project provider | PR threads, labels, statuses, source-branch deletion |
| `github:pr:merge` | the project provider, as the landing authority | PR completion and auto-complete |
| `provider:pr:write`, `provider:ci:cancel`, `repo:push` | the project provider | already provider-neutral |

On Azure DevOps an undeclared capability means no credential, as on GitHub:
each stage builds its provider from the credential of the capability it
declared, and from no other.

Two `ado:*` names are honoured when declared, and neither is required:
`ado:pr:complete` (completion uses its credential instead of
`github:pr:merge`'s) and `ado:pr:status`. The other four have no consumer in
DSL 2.0. They stay valid, so configurations that followed older docs keep
loading, but `goobers validate` reports `CAP006` and names what authorizes the
operation:

| Declared | What authorizes the operation |
| --- | --- |
| `ado:code:read` | No capability: repository reads use the repository credential. |
| `ado:pr:comment` | `github:pr:write` |
| `ado:pr:write` | `github:pr:write` |
| `ado:work-items:write` | `github:issues:write` (`github:issues:read` for reads). Not reported on `open-pr`, which consumes it to link the pull request to its work item. |

`CAP006` is strict-neutral: `goobers validate --strict` prints it but does not
fail on it. It covers DSL 2.0 workflows and the goobers their agentic tasks and
gates run. DSL 3.0 is out of scope; its provider-neutral names are designed in
`docs/design/provider-access-layer.md`. The full rule is in
`docs/design/ado-parity-dsl-2-0.md` §3.1.

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
