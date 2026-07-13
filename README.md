# Goobers

Upstream platform monorepo for **Goobers** — an open, self-hosted agent-workforce
platform. It starts as a single binary running a gaggle of AI agents against your
repo and backlog on one machine, and scales — without changing a definition — to
clustered orchestration over a large monorepo.

- **Architecture of record:** [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — one
  system across three deployment tiers; local runner first, cloud (Temporal/AKS) as
  drop-ins behind named seams.
- **Product vision:** [`docs/VISION.md`](docs/VISION.md)
- **Requirements:** [`docs/requirements/`](docs/requirements/)
- **Roadmap:** GitHub milestones — **V0** "works locally, begins to build itself",
  **V1** arbitrary repos/teams/hardening, **V2** cloud scale.

## Repository layout

| Path | Contents | Status |
|---|---|---|
| `api/` | Definition types, JSON invocation/result/verdict envelopes, YAML schema | Active — extended by DSL v0 |
| `providers/` | Backlog + repo provider abstraction (GitHub / ADO) | Active — V0 workload |
| `cmd/` | Binary entrypoints | Being consolidated into the `goobers` binary (V0) |
| `internal/` | Shared Go packages (engine core, telemetry, app bootstrap) | Active |
| `infra/` | Bicep, ArgoCD, Temporal, ADX | Quarantined — tier-3 drop-ins, revived in V2 |
| `portal/` | TypeScript + React observability portal | Active — retargets to run journals in V1 |
| `config-examples/` | Reference config layout + starter definitions | Active |
| `test/` | CI + e2e harness | Active |

See `docs/ARCHITECTURE.md §11` for the full disposition map.

## Go module

- Module path: `github.com/goobers/goobers`
- Minimum Go version: **1.23**

Import shared packages as e.g. `github.com/goobers/goobers/internal/version`.

## Binaries (`cmd/`)

The product binary is **`goobers`** — the local runner: `init`, `validate`, `up`
(daemon: scheduler + runner), `run`, `status`, `trace`. It is being built under the
V0 milestone; see the V0 epic issue for the work breakdown.

Pre-existing entrypoints (`operator`, `scheduler`, `goober-runtime`) are tier-3 /
superseded skeletons kept per the quarantine plan (`docs/ARCHITECTURE.md §11`).
Every binary shares `internal/app.Main`, which wires `--version`, structured logging
(`--log-level`, `--log-format`), and SIGINT/SIGTERM-aware shutdown.

## Quickstart (tier 1, local)

```sh
go build -o bin/goobers ./cmd/goobers    # or: make build

bin/goobers init ./my-instance           # scaffold an instance root
bin/goobers validate ./my-instance       # check instance.yaml + config/
```

`goobers init` scaffolds the instance root described in
`docs/ARCHITECTURE.md §6` — `instance.yaml`, `config/` (seeded with a starter
gaggle/goober/workflow), `runs/`, `scheduler/`, `workcopies/`, and a
`telemetry.db` placeholder — and is safe to re-run (existing pieces are left
untouched). Edit `instance.yaml` to point at your own repo and set the
referenced token env var or file; edit `config/` to shape your workforce.
`goobers up` (the daemon: scheduler + runner) lands in a later V0 mission.

## Developing

```sh
make help        # list targets
make build       # build all cmd/* into bin/
make test        # unit tests with race detector + coverage
make ci          # full local gate: fmt-check, vet, build, test, lint
```

CI runs the same gate on every PR to `main`.
