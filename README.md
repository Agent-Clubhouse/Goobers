# Goobers

Upstream platform monorepo for **Goobers** — an open, self-hosted agent-workforce
platform. It starts as a single binary running a gaggle of AI agents against your
repo and backlog on one machine, and scales — without changing a definition — to
clustered orchestration over a large monorepo.

- **Architecture of record:** [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — one
  system across three deployment tiers; local runner first, cloud (Temporal/AKS) as
  drop-ins behind named seams.
- **Concepts:** [`docs/concepts/`](docs/concepts/) — desired state, the config
  repository as source of truth, and the propose-via-PR trust model.
- **Product vision:** [`docs/VISION.md`](docs/VISION.md)
- **Requirements:** [`docs/requirements/`](docs/requirements/)
- **Roadmap:** GitHub milestones — **V0** "works locally, begins to build itself",
  **V1** arbitrary repos/teams/hardening, **V2** cloud scale.

## Repository layout

| Path | Contents | Status |
|---|---|---|
| `api/` | Definition types, JSON invocation/result/verdict envelopes, YAML schema | Active — extended by DSL v0 |
| `providers/` | Backlog + repo provider abstraction (GitHub / ADO) | Active — V0 workload |
| `cmd/goobers` | The product binary: `init`, `validate`, `up`, `run`, `status`, `trace` | Active — being built under V0 |
| `cmd/operator` | Kubernetes operator entrypoint | **Quarantined** — tier-3, revived in V2 |
| `cmd/scheduler` | Cluster scheduler process (Temporal-backed) | **Quarantined** — tier-3, revived in V2 |
| `cmd/goober-runtime` | Per-run agent pod runtime | **Superseded** — folds into `goobers`' local stage execution |
| `internal/operator` | Kubernetes operator reconcile logic | **Quarantined** — tier-3, revived in V2 |
| `internal/configsync` | Config-repo → CRD render/apply (ArgoCD bridge) | **Quarantined** — tier-3 (CRD-apply path), revived in V2 |
| `internal/` (other) | Shared Go packages (engine core, telemetry, app bootstrap) | Active |
| `infra/` | Bicep, ArgoCD, Temporal, ADX | **Quarantined** — tier-3 drop-ins, revived in V2 |
| `portal/` | TypeScript + React observability portal | Active — retargets to run journals in V1 |
| `config-examples/` | Reference config layout + starter definitions | Active |
| `skills/` | Portable agent skills for authoring Goobers config | Active |
| `test/` | CI + e2e harness | Active |

Quarantined paths stay in-tree, compiling, and status-bannered — they are the
documented tier-3 drop-in points (`docs/ARCHITECTURE.md §10`), not dead code.
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

New to declarative control systems? Read
[How Goobers works](docs/concepts/README.md) first; it explains why `config/`
defines behavior while runs, workcopies, and scheduler records are runtime
state.

```sh
go build -o bin/goobers ./cmd/goobers    # or: make build

bin/goobers init ./my-instance           # scaffold an instance root
bin/goobers validate ./my-instance       # check instance.yaml + config/
bin/goobers config show ./my-instance    # render the effective config (secrets redacted)
bin/goobers run default-implement ./my-instance   # trigger a run manually
bin/goobers status ./my-instance         # list runs + their phase
bin/goobers claims list ./my-instance    # inspect current claim leases
bin/goobers claims release --force <item-id> ./my-instance # override a live holder
# If an item ID is claimed in multiple namespaces, add:
#   --gaggle=<name> --provider=<name>
bin/goobers trace <run-id> ./my-instance # inspect one run's journal
bin/goobers escalations ./my-instance    # list escalated runs
bin/goobers escalations show <run-id> ./my-instance # inspect cause + artifact timeline
```

`goobers init` scaffolds the instance root described in
`docs/ARCHITECTURE.md §6` — `instance.yaml`, `config/` (seeded with a starter
gaggle/goober/workflow), `runs/`, `scheduler/`, `workcopies/`, and a
`telemetry.db` placeholder — and is safe to re-run (existing pieces are left
untouched). Edit `instance.yaml` to point at your own repo and set the
referenced token env var or file; edit `config/` to shape your workforce.
`goobers up` runs the daemon (embedded scheduler + local runner): it restarts
any run interrupted by a prior crash via `Runner.Resume`, then drives
scheduled workflows until interrupted, draining in-flight runs gracefully on
SIGINT/SIGTERM. `run` remains the way to trigger one workflow manually
without a daemon running. Full walkthrough:
[`docs/guides/quickstart.md`](docs/guides/quickstart.md). Platform-specific
setup: [Linux quickstart](docs/guides/quickstart-linux.md),
[Windows quickstart](docs/guides/quickstart-windows.md) (install & verify); run
the daemon as a supervised service via
[Daemon supervision](docs/guides/supervision.md)
(systemd · launchd · Windows Service). How binaries are built, packaged, and
verified for distribution: [Releases & packaging](docs/guides/releases.md).
Azure DevOps instances can use
[Azure CLI, workload identity, managed identity, or PAT authentication](docs/guides/ado-authentication.md).

## Authoring workflow DSL with an agent

Use the portable
[`goobers-dsl-author` skill](skills/goobers-dsl-author/SKILL.md) from your own
agent harness to find the canonical DSL docs, learn the core terms, and turn a
plain-English process into schema-valid gaggle, goober, and workflow YAML. It
does not require a running Goobers daemon. See the
[installation and usage guide](docs/guides/dsl-authoring-skill.md).

## Shell completion

Enable subcommand and flag completion, plus instance-aware workflow and recent
run ID completion, with the line for your shell (add it to the shell's startup
file to make it permanent):

```sh
source <(goobers completion bash)  # bash
source <(goobers completion zsh)   # zsh
goobers completion fish | source  # fish
```

## Developing

```sh
make verify-fast # pre-push format, vet, and Go build tier
make tidy-check  # check that go.mod/go.sum match tidy output
make ci          # merge gate (Go + config + portal)
make verify-full # merge plus integration, platform, and coverage gates
make vulncheck   # scan reachable Go code for known vulnerabilities
```

CI runs the same merge-tier implementation on every PR to `main`. See the
[`validation tier contract`](CONTRIBUTING.md#validation-tier-contract) for
audience guidance, CI job mapping, and per-platform prerequisites.

## Contributing

Goobers is open source and contributions are welcome. See
[`CONTRIBUTING.md`](CONTRIBUTING.md) for the workflow, [`SECURITY.md`](SECURITY.md)
for vulnerability disclosure, and the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

Licensed under the [MIT License](LICENSE).
