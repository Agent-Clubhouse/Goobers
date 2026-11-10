# Developing Goobers

This guide covers the source-tree layout, local validation, and implementation
details for contributors. Product installation and first-run instructions live
in the [README](README.md).

## Repository layout

| Path | Contents | Status |
| --- | --- | --- |
| `api/` | Definition types, invocation/result/verdict envelopes, and YAML schema | Active |
| `cmd/goobers` | Product binary: `init`, `validate`, `up`, `run`, `status`, and `trace` | Active |
| `providers/` | Backlog and repository providers for GitHub, Azure DevOps, and Gitea (experimental) | Active |
| `internal/` | Engine, journal, scheduler, telemetry, and application packages | Active |
| `portal/` | TypeScript and React observability portal | Active |
| `reference-workflows/` | Canonical workflows used to operate Goobers against this repository | Active |
| `config-examples/` | Reference configuration layout and starter definitions | Active |
| `examples/` | Specialized end-to-end examples | Active |
| `samples/` | Versioned disposable onboarding targets | Active |
| `release/` | Release archives, installer, notes, metadata, and packaging tools | Active |
| `packaging/` | Container build and embedded service-supervisor assets | Active |
| `agent-toolkit/` | Release-owned bundle instructions and harness adapters | Active |
| `skills/` | Canonical portable skills used by the agent toolkit | Active |
| `test/` | CI, integration, end-to-end, and coverage harnesses | Active |
| `config/` | CRD manifests for tier-3 configuration delivery | Quarantined |
| `cmd/operator`, `internal/operator` | Kubernetes operator entrypoints and reconciliation | Quarantined |
| `internal/configsync` | Configuration repository to CRD rendering and apply path | Quarantined |
| `infra/`, `deploy/` | Cloud-scale infrastructure and customer-applied reference manifests | Reference or quarantined |
| `evals/` | EvalSuite (#2662) design and Python prototype. **Direction superseded** by #2681; retained pending ratification — see [`docs/design/evals-suite.md`](docs/design/evals-suite.md) | Historical |

Quarantined paths remain in-tree and compiling as documented cloud-scale
extension points; they are not part of the shipped local runner. See
[Architecture section 11](docs/ARCHITECTURE.md) for the full disposition map.

## Toolchain

- Go version: use the toolchain declared in [go.mod](go.mod).
- Module path: `github.com/goobers/goobers`.
- Portal dependencies: Node.js and npm versions supported by the CI workflows.

Import shared packages with the module path, for example
`github.com/goobers/goobers/internal/version`.

## Build and validate

```sh
make build       # build the goobers binary
make verify-fast # pre-push format, vet, and Go build tier
make tidy-check  # verify go.mod and go.sum match tidy output
make ci          # merge gate: Go, configuration, portal, and workflow contracts
make verify-full # merge gate plus integration, platform, and coverage checks
make vulncheck   # scan reachable Go code for known vulnerabilities
```

Use the smallest relevant command while iterating, then run the validation tier
required by [CONTRIBUTING.md](CONTRIBUTING.md#validation-tier-contract) before
opening a pull request.

## Binaries

The shipped product binary is `goobers`. It runs the local scheduler and runner
and exposes instance setup, validation, execution, status, and trace commands.

The `operator` and `config-sync` entrypoints are cloud-scale skeletons retained
under the architecture quarantine plan. They are not current deployment paths.

Every binary uses `internal/app.Main` for version output, structured logging,
and signal-aware shutdown.

The quarantined `operator` binary accepts these environment overrides:

| Variable | Default | Purpose |
| --- | --- | --- |
| `GOOBERS_OPERATOR_METRICS_ADDR` | `:8080` | Metrics bind address; `0` disables metrics |
| `GOOBERS_OPERATOR_HEALTH_ADDR` | `:8081` | Health/readiness probe bind address; `0` disables probes |
| `GOOBERS_OPERATOR_LEADER_ELECTION` | `false` | Enable leader election with `true` |
| `GOOBERS_OPERATOR_WORKER_IMAGE` | `ghcr.io/goobers/goober-runtime:latest` | Image for reconciled worker Deployments |

Unset or empty values keep the defaults. An invalid leader-election boolean
fails startup. The default worker image remains an unpublished placeholder;
these overrides do not remove the operator's quarantine or enable its reference
Deployment, which remains at zero replicas.

## Portal development

See [portal/README.md](portal/README.md) for the frontend development loop.

Append `?portal-diagnostics=1` before the hash route, for example
`http://localhost:5173/?portal-diagnostics=1#/runs`, to enable an ephemeral
`console.debug` stream with request timing, overlapping-request counts, and SSE
connection events. Diagnostics are disabled by default, are not persisted, and
are not partner-facing workflow telemetry.

## Generated documentation and releases

CLI reference, man pages, and completions are generated from the command
registry. Release packaging also adapts the source README and quickstart into a
release-pinned documentation bundle. When changing those source sections, keep
the rewrite contracts in `release/docs.go` aligned and run the relevant release
tests.

See [Releases and packaging](docs/guides/releases.md) for the distribution
pipeline and release artifact contract.
