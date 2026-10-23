# Cloud operations

`goobers-cloud` operator commands address the control-plane API with `--api`
or `GOOBERS_CLOUD_API`. Set `GOOBERS_API_TOKEN` when the API requires bearer
authentication. Commands print the API's JSON response so scripts and humans
see the same bounded representation.

Commands whose PostgreSQL control-plane operation has not landed fail closed
and name the owning issue. They do not read an instance root or simulate a
successful cloud operation.

## Local-to-cloud command map

Workflow-stage and connector commands are intentionally absent: cloud workers
run those stages. The table maps the instance-root and operator commands.

| `goobers` command | Cloud equivalent | Notes |
|---|---|---|
| `status` | `goobers-cloud status --api URL` | Instance/API status. |
| `runs list` | `goobers-cloud runs list --api URL` | Supports gaggle, workflow, phase and limit filters. |
| `trace RUN` | `goobers-cloud runs logs --api URL RUN` | Reads journal events; use `runs show` for summary state. |
| `claims active`, `claims list` | `goobers-cloud claims list --api URL` | Active cloud claims. |
| `claims release` | `goobers-cloud claims release` | Fails closed until #7808 (implementation PR #7845). |
| `run`, `signal` | `goobers-cloud triggers --api URL ...` | Durable API trigger with an idempotency key. |
| `approve` | `goobers-cloud approve --api URL ...` | Runtime intervention. |
| `override` | `goobers-cloud override --api URL ...` | Runtime intervention. |
| `escalations resolve --resolution=deny` | `goobers-cloud deny --api URL ...` | Denies an escalated run. |
| `run cancel` | `goobers-cloud cancel --api URL ...` | Idempotent live-run cancellation. |
| `apply` | `goobers-cloud config apply` | Fails closed until #7834. |
| `config` | `goobers-cloud config status` | Fails closed until #7834. |
| migration inspection | `goobers-cloud migrate status` | Reads applied migrations and minimum-compatible binary state. |
| daemon ownership/status | `goobers-cloud lease status` | Reads holder, epoch, DB-clock expiry and recent lease history. |
| `worker` | `goobers-cloud worker` | Cloud role, normally run by the deployment. |
| `completion` | `goobers-cloud completion` | Generates Bash, Fish or Zsh completion. |
| `version`, `versions` | deployment image/version metadata | No instance-root operation. |
| `init`, `connect`, `scaffold`, `examples` | configuration repository tooling | Not applicable to a running cloud instance. |
| `validate`, `lint`, `fix`, `schema`, `explain`, `features`, `workflow show`, `workflow validate-child` | CI/config-repository tooling | Run before `config apply`; not a control-plane mutation. |
| `up`, `down`, `service`, `service task-*`, `self-update` | Kubernetes deployment lifecycle | Not applicable; operate the Deployment. |
| `dashboard` | Portal service | Not a CLI operation. |
| `cost`, `stats`, `telemetry` | OTLP/cloud telemetry tools | Local SQLite analytics are not available in cloud. |
| `escalations`, `escalations show` | `runs show` / Portal | Dedicated cloud listing is not yet exposed. |
| `run abort`, `rerun-stage` | API intervention or cancellation | No direct cloud equivalent in this slice. |
| `runs du`, `workspace`, `recovery-*`, `journal redact` | object-store/retention administration | Instance-root storage commands are not applicable. |
| `roots *`, `blocked *`, `queue-explain` | database-backed fleet operations | No instance-root equivalent. |
| `doctor`, `preflight`, `diagnostics` | deployment/cluster diagnostics | Run the cloud deployment diagnostics, not local-root checks. |
| `gaggle *`, `config-seed`, `onboarding` | configuration repository pipeline | Not applicable to the live control plane. |
| `agent-kit`, `portal-extension`, `harness`, `speech` | user workstation tooling | Not applicable. |
| `fleet` | superseded | Not applicable. |
| `temporal` | Temporal administration tooling | Operate Temporal directly with its supported tools. |
| `reset-rate-limit` | none | Cloud admission budgets are database-backed; no equivalent is exposed yet. |
| `worker`, `netpol-render`, `e2e *` | deployment engineering | Not day-to-day operator API commands. |
| `work-items`, `pr retry`, `advisory-pr-reset` | workflow/provider operations | Run through managed workflows, not the control-plane CLI. |
| workflow-stage commands shown by `goobers help stages` | none | Executed by cloud workers; never invoked as operator commands. |

## Exit behavior

- `0`: the API accepted the operation or returned the requested data.
- `1`: the API refused the operation, or the command is explicitly unavailable
  until its owning issue.
- `2`: usage, API URL, transport, encoding, or response-read failure.

Mutation commands require `--idempotency-key`. Retrying the same command with
the same key asks for the same durable outcome; changing the key asks for a new
operation.
