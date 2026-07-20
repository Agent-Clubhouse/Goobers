# Goobers DSL authoring reference

This is a portable checklist, not a replacement for the schemas in the target
Goobers release. Links resolve when this skill is used from its source
checkout.

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
metadata:
  name: lowercase-dns-style-name
spec: {}
```

Definition schemas reject unknown fields. `metadata.name` starts and ends with
a lowercase letter or digit and otherwise uses lowercase letters, digits, and
hyphens.

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
- `harness: copilot`, an optional supported `model`, and `harnessOptions`;
- `capabilities`, `skills`, and `tools`;
- `scaleFactor`;
- `workflows` when the associations are known.

The `instructions` path is relative to the goober definition directory. Keep
the role, scope, completion contract, and safety limits in that markdown file;
keep harness, tools, grants, scale, and workflow association in YAML.

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
| `backlog-item` | `selector`, with string values. |
| `signal` | `signal`, naming the external signal. |

A deterministic task requires `name`, `type: deterministic`, `goal`, and
`run.command`; it must not set `goober`. An agentic task requires `name`,
`type: agentic`, `goal`, and `goober`; it must not set `run`.

Values under `inputs` and `inputsFrom` are strings. Use `expectedOutputs` for
the scalar outputs or artifact names a later state relies on. A normal
successful terminal task omits `next`; `@abort` and `@escalate` are explicit
non-success terminals.

A gate has `name`, exactly one evaluator configuration, and `branches`.
Automated checks currently include `status-equals`, `output-equals`,
`output-not-equals`, `output-numeric-gte`, `output-numeric-lte`,
`output-numeric-lt`, `output-matches`, `ci-status`, `land-outcome`, and
`queue-outcome`. Use only outcomes and parameters accepted by the target
release. Agentic gates must cover `pass`, `fail`, and `needs-changes`. Human
gates may be present in a schema before a runner supports them, so always
confirm them with `goobers validate`.

## Canonical capabilities

Use only the target release's registry. The current set is:

| Capability | Grant |
|---|---|
| `repo:read` | Read-only target-repository checkout. |
| `repo:push` | Push the run branch to the target repository. |
| `github:issues:write` | Query, create, label, close, or comment on GitHub issues. |
| `github:pr:write` | Open, inspect, update, or close GitHub pull requests. |
| `github:pr:review` | Submit provider-native pull-request reviews. |
| `github:branch:delete` | Delete a remote GitHub branch. |
| `github:pr:merge` | Merge a GitHub pull request. |
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
- Every `start`, `next`, and branch target exists or is a reserved terminal.
- Every task and gate is reachable from `start`.
- Trigger fields match the trigger type; schedules parse.
- Task type matches `run` versus `goober`.
- Gate evaluator block and outcome vocabulary match the evaluator.
- Capabilities are canonical and agentic task grants are a subset of goober
  grants.
- Instructions files and referenced scripts exist at the paths expected by
  their execution workspace.
- Secrets are references, not values.
- Structured data is an artifact; result outputs remain scalar.

Finish with `goobers validate` so schema and compiler checks both run.
