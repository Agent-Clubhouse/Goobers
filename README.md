# Goobers

Upstream platform monorepo for **Goobers** — an agent workforce platform.
Product vision and requirements live in [`docs/`](docs/) (start with
[`docs/VISION.md`](docs/VISION.md) and [`docs/requirements/`](docs/requirements/)).

## Repository layout

| Path | Contents | Owner |
|---|---|---|
| `api/` | CRD types, JSON invocation/result/verdict envelopes, YAML schema | Dev-1 |
| `providers/` | Backlog + repo provider abstraction (GitHub / ADO) | Dev-2 |
| `cmd/` | Control-plane binary entrypoints | Dev-3 |
| `internal/` | Shared Go packages (version, signals, app bootstrap) | Dev-3 |
| `infra/` | Bicep, ArgoCD, Temporal, ADX | Dev-3 |
| `portal/` | TypeScript + React observability portal | Dev-4 |
| `config-examples/` | Reference config-repo layout + examples | Dev-1 |
| `test/` | CI + e2e harness | QA |

## Go module

- Module path: `github.com/goobers/goobers`
- Minimum Go version: **1.23**

Import shared packages as e.g. `github.com/goobers/goobers/internal/version`.

## Control-plane binaries (`cmd/`)

| Binary | Role |
|---|---|
| `operator` | Reconciles Goobers CRDs into replicas + Temporal registrations (DEP-012) |
| `scheduler` | Routes backlog items to gaggles; drives work-claiming |
| `goober-runtime` | Per-run agent runtime inside an ephemeral pod (DEP-004..007) |

These are skeleton entrypoints today: they boot, log, and block until signalled.
Domain logic is added by follow-on missions. Every binary shares
`internal/app.Main`, which wires `--version`, structured logging
(`--log-level`, `--log-format`), and SIGINT/SIGTERM-aware shutdown.

## Developing

```sh
make help        # list targets
make build       # build all cmd/* into bin/
make test        # unit tests with race detector + coverage
make ci          # full local gate: fmt-check, vet, build, test, lint
```

CI runs the same gate via [`azure-pipelines.yml`](azure-pipelines.yml) on every
PR to `main`.
