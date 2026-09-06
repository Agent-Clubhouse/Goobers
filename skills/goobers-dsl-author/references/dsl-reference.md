# Goobers DSL authoring reference

This is a portable checklist, not a replacement for the schemas in the target
Goobers release. Links resolve from either a source checkout or the mirrored
layout in a release-owned agent toolkit.

## Source map

| Need | Canonical source |
|---|---|
| Architecture and precedence | [`docs/ARCHITECTURE.md`](../../../docs/ARCHITECTURE.md) |
| Config model and layout | [`docs/requirements/config-as-code.md`](../../../docs/requirements/config-as-code.md), [`config-examples/`](../../../config-examples/) |
| Gaggle semantics and shape | [`docs/requirements/gaggle.md`](../../../docs/requirements/gaggle.md), [`gaggle.schema.json`](../../../api/schemas/gaggle.schema.json) |
| Goober semantics and shape | [`docs/requirements/goober.md`](../../../docs/requirements/goober.md), [`goober.schema.json`](../../../api/schemas/goober.schema.json) |
| Workflow, task, trigger, and gate shape | [`docs/requirements/workflow.md`](../../../docs/requirements/workflow.md), [`task.md`](../../../docs/requirements/task.md), [`gate.md`](../../../docs/requirements/gate.md), [`workflow.schema.json`](../../../api/schemas/workflow.schema.json) |
| Stage data and completion | [`docs/stage-contract.md`](../../../docs/stage-contract.md), invocation/result/artifact schemas under [`api/schemas/`](../../../api/schemas/) |
| Capability strings | [`internal/capability/capability.go`](../../../internal/capability/capability.go) |

## Resource rules

Every definition is a Kubernetes-style resource:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Gaggle | Goober | Workflow
dslVersion: "release-supported version" # Workflow only
metadata:
  name: lowercase-dns-style-name
spec: {}
```

Definition schemas reject unknown fields. `metadata.name` starts and ends with
a lowercase letter or digit and otherwise uses lowercase letters, digits, and
hyphens. Workflows should explicitly select a DSL version listed by the
toolkit's `release.json` or `goobers versions --json`.

### Manifest and local instance

`manifest.yaml` owns every named connection referenced by a gaggle. A minimal
manifest for a GitHub project and backlog is:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: acme-config
spec:
  instance:
    name: acme
    environment: dev
  connections:
    - name: github-repo
      type: repo
      provider: github
      secretRef:
        name: github-token
    - name: github-backlog
      type: backlog
      provider: github
      secretRef:
        name: github-token
  gaggles:
    - acme-api
```

For a tier 1-2 source tree, `instance.yaml.example` separately declares target
repos and the concrete env/file credential sources needed by capabilities:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: acme
    name: api
    token:
      env: GOOBERS_GITHUB_TOKEN
credentials:
  - capability: agent:model
    token:
      env: GOOBERS_COPILOT_TOKEN
```

`instance.yaml(.example)` has no `connections` field and does not define names
for gaggle lookup. Add or omit capability credential grants according to the
generated tasks; never invent a credential value.

### Gaggle

Required semantic content:

- `spec.project`: `provider` (`github` or `ado`), `owner`, `name`, and normally
  `branch` plus `connectionRef`;
- `spec.backlog`: `provider`, `project`, and normally `labels` or `query` plus
  `connectionRef`;
- `spec.isolation.namespace`.

The gaggle has one primary project and one singleton backlog. Connection names
refer only to entries under `manifest.yaml`'s `spec.connections`; credentials
never appear in the gaggle. Every non-empty project or backlog `connectionRef`
must resolve there. Use a `repo` connection for the project and a `backlog`
connection for the backlog.

### Goober

Provide:

- `gaggle`, `role`, and `instructions`;
- a supported `harness`, an optional supported `model`, and `harnessOptions`;
- `capabilities`, `skills`, and `tools`;
- `scaleFactor`;
- `workflows` when the associations are known.

| Harness | Command | `agent:model` credential mapping |
|---|---|---|
| `copilot` | `copilot` | `COPILOT_GITHUB_TOKEN` |
| `claude-code` | `claude` | `ANTHROPIC_API_KEY` |

The harness credential is optional when its CLI already has an interactive
sign-in. When a headless instance supplies `agent:model`, the runner maps that
capability to the selected harness's environment variable above. Claude Code
accepts `effort` under `harnessOptions`; Copilot and Claude Code model names are
validated by their respective adapters.

The `instructions` path is relative to the goober definition directory. Keep
the role, scope, completion contract, and safety limits in that markdown file;
keep harness, tools, grants, scale, and workflow association in YAML.
Set `harnessOptions.fallback-to-default: true` only when using the harness
default is preferable to rejecting an unavailable requested model.

### Workflow

Provide:

- `gaggle`;
- one or more `triggers`;
- `readiness`, normally beginning with `maxConcurrentRuns: 1`;
- `start`, naming an existing task or gate;
- `tasks` and `gates` needed by the state graph.

Trigger-specific fields:

| Type | Fields |
|---|---|
| `manual` | No schedule, signal, or selector; it must be the only trigger. |
| `schedule` | `schedule`, quoted as a cron expression or supported descriptor. |
| `backlog-item` | `selector`, with string values. `goobers up` treats selector keys as required backlog labels and dispatches eligible runs subject to readiness limits; selector values are not used for GitHub label matching. |
| `signal` | `signal`, naming the external signal. |

For autonomous V0 backlog consumption, use a schedule and make the first task
claim one eligible item:

```yaml
triggers:
  - type: schedule
    schedule: "0 * * * *"
start: query-backlog
tasks:
  - name: query-backlog
    type: deterministic
    goal: Claim one approved backlog item.
    run:
      command: ["goobers", "backlog-query", "--claim"]
    inputs:
      trustLabel: "goobers"
      maxItems: "1"
      resultFile: "claimed-item.json"
    capabilities:
      - github:issues:write
    expectedOutputs:
      - claimed-item
```

When another state processes the claimed item, add `next` naming that reachable
task or gate. On public repos, `trustLabel` must name a maintainer-applied trust
label. The architecture-recommended V0 pattern is the schedule above. The
current local daemon also polls and dispatches workflows with a `backlog-item`
trigger, but its poll only counts eligible items; it does not claim one. Such a
workflow should therefore also start with a deterministic `goobers
backlog-query --claim` task.

A deterministic task requires `name`, `type: deterministic`, `goal`, and
`run.command`; it must not set `goober`. An agentic task requires `name`,
`type: agentic`, `goal`, and `goober`; it must not set `run`.

Values under `inputs` and `inputsFrom` are strings. Use `expectedOutputs` for
the scalar outputs or artifact names a later state relies on. A normal
successful terminal task omits `next`; `@abort` and `@escalate` are explicit
non-success terminals.

An agentic task that produces a rich, freeform artifact (a report, generated
doc — anything beyond a scalar output) should declare `inputs: {artifactFile:
<name>}` rather than have the model write the file with a generic tool and
self-report the path. This makes the stage automatically eligible for the
`goobers-io` MCP's `publish_output` tool, and the resulting artifact
propagates to the *next* stage automatically (no `context:` YAML key exists
— there is nothing else to declare), making that stage eligible for
`goobers-io`'s `list_inputs`/`read_input`/`grep_input` read tools over it.
Never declare `goobers-io` as an `mcpServers`/`tools:` entry by hand — it is
auto-wired from `artifactFile`/propagated context alone. See
[the goobers-io MCP guide](../../../docs/guides/goobers-io-mcp.md) for the
full mechanics and what to put (and not put) in `instructions.md`.

A gate has `name`, exactly one evaluator configuration, and `branches`.
Automated checks currently include `status-equals`, `failure-class`,
`output-equals`, `output-not-equals`, `output-numeric-gte`,
`output-numeric-lte`, `output-numeric-lt`, `output-matches`, `ci-status`,
`land-outcome`, and `queue-outcome`. `failure-class` takes no parameters: it
returns `pass` for success, `infra` for a retryable failure, and `fail` for
every other status. Use only outcomes and parameters accepted by the target
release. Agentic gates must cover `pass`, `fail`, and `needs-changes`. Human
gates may be present in a schema before a runner supports them, so always
confirm them with `goobers validate`.

## Canonical capabilities

Use only the target release's registry. The current set is:

| Capability | Grant |
|---|---|
| `repo:read` | Read-only target-repository checkout. |
| `repo:push` | Push the run branch to the target repository. |
| `github:issues:read` | Query GitHub issues without mutation authority. |
| `github:issues:write` | Query, create, label, close, or comment on GitHub issues. |
| `github:milestones:write` | Assign existing GitHub milestones to selected issues. |
| `github:issues:approve` | Apply the trusted `goobers:approved` issue label. |
| `provider:pr:write` | Perform pull-request operations through the configured repository provider. |
| `github:pr:write` | Open, inspect, update, or close GitHub pull requests. |
| `github:pr:review` | Submit provider-native pull-request reviews. |
| `provider:ci:cancel` | Cancel bounded pending provider CI only for an exact reviewed pull-request head. |
| `github:branch:delete` | Delete a remote GitHub branch. |
| `github:pr:merge` | Merge a GitHub pull request. |
| `contents:read` | Fetch a separately declared reference repository with its repo-scoped read credential. |
| `ado:code:read` | Inspect Azure Repos code and pull requests read-only. |
| `ado:pr:comment` | Post Azure Repos pull-request threads without voting or completing. |
| `ado:pr:write` | Open and update Azure Repos pull requests (no completion or merge authority). |
| `ado:pr:status` | Publish Azure Repos pull-request statuses that branch policies gate on. |
| `ado:pr:complete` | Complete (merge) an Azure Repos pull request; the ADO counterpart to `github:pr:merge`. |
| `ado:work-items:write` | Update explicitly selected Azure Boards work items. |
| `telemetry:read` | Read the Goobers telemetry rollup. |
| `journal:read` | Resolve evidence from another run's journal. |
| `agent:model` | Supply an agentic harness with its model credential. |

Grant the minimum set. Deterministic tasks declare their own required
capabilities. For an agentic task, each task capability must also appear in the
referenced goober's capability list.

## Pre-validation checklist

- All resource and state names are unique and valid.
- The manifest includes every newly added gaggle.
- Every gaggle `connectionRef` resolves to a named `spec.connections` entry in
  the manifest with the appropriate connection type.
- `instance.yaml(.example)` lists the target repos and env/file credential
  sources required by the generated capabilities; it has no named
  `connections` field.
- Gaggle, goober, and workflow references agree exactly.
- Every workflow has an explicit `dslVersion` supported by the target release.
- Every `start`, `next`, and branch target exists or is a reserved terminal.
- Every task and gate is reachable from `start`.
- Trigger fields match the trigger type; schedules parse.
- A V0 autonomous backlog workflow has a schedule trigger and starts with a
  deterministic `goobers backlog-query --claim` task. If it instead uses the
  local daemon's `backlog-item` trigger, its first task still claims one item.
- Task type matches `run` versus `goober`.
- Gate evaluator block and outcome vocabulary match the evaluator.
- Capabilities are canonical and agentic task grants are a subset of goober
  grants.
- Instructions files and referenced scripts exist at the paths expected by
  their execution workspace.
- Secrets are references, not values.
- Structured data is an artifact; result outputs remain scalar.

Finish with `goobers validate` so schema and compiler checks both run.
