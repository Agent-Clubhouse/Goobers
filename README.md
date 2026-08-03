# Goobers

**Goobers is an open, self-hosted platform for running an AI workforce against
your repositories and backlog.** Instead of giving one agent an open-ended
prompt, you define a team of roles (a *gaggle*) and the workflow, permissions,
checks, retry limits, and human handoffs that govern its work.

It is for a solo builder who wants dependable issue-to-PR automation, a small
team that wants agents to work within its existing review and CI policy, or a
larger organization that needs the same definitions to remain useful as its
execution infrastructure grows. Today the shipped runner is one Go binary that
runs on one machine. Cloud-scale orchestration is an explicit design goal, not a
current product claim; the cluster entrypoints in this repository remain
quarantined. What is intended to stay constant is the configuration and run
contract, so scaling execution does not require redefining the workforce.

## Predictable workflows around nondeterministic workers

A Goobers workflow is YAML that declares triggers, stages, gates, transitions,
capabilities, retries, and budgets. The loader validates that definition and
[`internal/workflow` compiles it into a pinned state machine](internal/workflow/compile.go);
the runner walks that machine rather than asking an agent what should happen
next. Stages are either:

- **deterministic**, such as querying a backlog, running tests, polling CI, or
  opening a pull request; or
- **agentic**, where a named goober receives an invocation envelope and must
  return a result envelope under explicitly granted capabilities.

Gates turn results into declared branches. They can run an automated check, ask
an independent reviewer agent for a verdict, or pause for human approval. A
failure can route back through a bounded repair loop, stop the run, or
[`@escalate` for human attention](internal/workflow/machine.go). The shipped
[`implementation` workflow](reference-workflows/gaggles/goobers/workflows/implementation.yaml)
is a concrete definition: it claims one approved issue, gives an isolated
worktree to an implementer, routes review findings and local-CI failures back to
that implementer, and only then pushes a branch and opens a PR.

This makes the agent useful without making it the control plane:

- a lease-based [claim ledger](internal/localscheduler/claim.go) prevents two
  runs from silently taking the same backlog item;
- each run pins its workflow definition and records an
  [append-only, content-digested journal](internal/journal/doc.go) of inputs,
  stage and gate events, and artifacts; and
- exhausted or explicitly escalated paths become visible run outcomes and can
  [notify the driving backlog item](internal/gate/escalate.go).

The result is inspectable automation: the model's answer can vary, but the
route it is allowed to take, the effects it can request, and the evidence left
behind are declared and reviewable.

## GitOps for the workforce itself

Goobers treats workforce definitions as desired state. Keep `manifest.yaml`,
gaggles, goobers, instructions, and workflows in a configuration repository;
protect its main branch with the same pull-request, CI, and CODEOWNERS rules you
use for infrastructure configuration. Agents may propose a definition change
on a branch, but repository governance decides whether it becomes active. They
do not rewrite the running workforce in place. See
[How Goobers works](docs/concepts/README.md) for the trust model.

An instance's `workflowSource` points at a Git branch (default: `main`).
The shipped [Git source](internal/instance/gitsource.go) fetches that branch's
committed tree and materializes an immutable snapshot. While `goobers up` is
running, the [reconcile loop](cmd/goobers/up.go) notices new commits by polling,
local-ref changes, or an authenticated push webhook. It atomically installs the
new definitions, validates them, and reloads the scheduler and runners. An
invalid revision is rejected and the last-known-good definitions continue
running ([reload implementation](cmd/goobers/configreload.go)). In other words:
merge reviewed desired state to the tracked branch and Goobers converges on it,
with an audit trail and a safe rejection path. It is the same operating model
Argo CD popularized for cluster configuration, applied to an agent workforce.

## One real agent-to-human handoff

[Issue #664](https://github.com/Agent-Clubhouse/Goobers/issues/664) requested a
flake ledger and quarantine policy, and was explicitly filed as backlog for
future investment, **not approved for automated implementation**. A running
Goobers instance claimed and worked it anyway, on
`goobers/implementation/a5236bf6de96406c933ecb1a9b9b83bc`--the branch name
identifies the workflow and run.

1. The deterministic backlog stage selected and leased an issue it should not
   have: #664 carried no automated-implementation authorization.
2. The implementation agent worked in the run's isolated branch; the reviewer
   and local-CI gates controlled whether it could advance or needed a repass.
3. Deterministic stages published the branch and opened
   [PR #1746](https://github.com/Agent-Clubhouse/Goobers/pull/1746). Review
   caught the authorization violation--not merely defects in the diff--and
   escalated to a human; that PR closed without merging.
4. A maintainer authorized the work directly, repaired the defects the review
   had also found, manually opened replacement
   [PR #2200](https://github.com/Agent-Clubhouse/Goobers/pull/2200), and
   merged it on 2026-08-01 after the resulting
   [CI run passed](https://github.com/Agent-Clubhouse/Goobers/actions/runs/30722079485),
   including the shipped-workflow contract checks.

This was not one fully automated issue-to-merge run: automation claimed work
it was not authorized to do, then a human corrected the authorization,
completed the remediation, and merged. It demonstrates the intended boundary
in concrete terms even when the boundary is first crossed by mistake: trusted
backlog work enters a declared machine; agents do bounded work; deterministic
stages and gates coordinate repository effects and expose unresolved
problems--including authorization violations, not just defects; and humans
take over with an ordinary branch, PR, and inspectable history rather than an
opaque agent session.

## Try it locally

There is no tagged release yet, so build with the Go toolchain declared in
[`go.mod`](go.mod). The fastest path is a credential-free demo on Linux or
macOS:

```sh
make build
bin/goobers init --demo ./demo-instance
bin/goobers run demo ./demo-instance
bin/goobers trace <run-id> ./demo-instance
```

The demo uses mock providers and performs no network writes. It exercises
curation, implementation, a review gate, and a merge preview, then leaves the
run journal for `trace` to inspect. The [full quickstart](docs/guides/quickstart.md)
then walks through a disposable GitHub-backed issue-to-PR run and a regular
instance. Production-oriented definitions live in
[`config-examples/`](config-examples/).

For deeper context, read the [product vision](docs/VISION.md),
[architecture of record](docs/ARCHITECTURE.md), [concepts](docs/concepts/), and
[requirements](docs/requirements/). Those documents include future design;
this overview deliberately describes the behavior shipped in this tree.

## Engineering reference

### Repository layout

| Path | Contents | Status |
|---|---|---|
| `api/` | Definition types, JSON invocation/result/verdict envelopes, YAML schema | Active |
| `providers/` | Backlog + repo provider abstraction (GitHub / ADO) | Active |
| `cmd/goobers` | The product binary: `init`, `validate`, `up`, `run`, `status`, `trace` | Active |
| `cmd/operator` | Kubernetes operator entrypoint | **Quarantined** — reserved for cloud-scale execution |
| `cmd/scheduler` | Cluster scheduler process (Temporal-backed) | **Quarantined** — reserved for cloud-scale execution |
| `cmd/goober-runtime` | Per-run agent pod runtime | **Superseded** — folds into `goobers`' local stage execution |
| `internal/operator` | Kubernetes operator reconcile logic | **Quarantined** — reserved for cloud-scale execution |
| `internal/configsync` | Config-repo → CRD render/apply (ArgoCD bridge) | **Quarantined** — CRD-apply path is not part of the shipped local runner |
| `internal/` (other) | Shared Go packages (engine core, telemetry, app bootstrap) | Active |
| `infra/` | Bicep, ArgoCD, Temporal, ADX | **Quarantined** — cloud infrastructure is not a shipped deployment path |
| `portal/` | TypeScript + React observability portal; operator co-brandable via `instance.yaml` | Active |
| `reference-workflows/` | Canonical, proven workflow configs used to operate Goobers against this repository | Active — tested dogfood reference |
| `config-examples/` | Reference config layout + starter definitions | Active |
| `samples/` | Versioned, disposable onboarding targets | Active |
| `agent-toolkit/` | Release-owned bundle instructions and harness adapters | Active |
| `skills/` | Canonical portable skills used by the agent toolkit | Active |
| `test/` | CI + e2e harness | Active |

Quarantined paths stay in-tree, compiling, and status-bannered — they are the
documented cloud-scale drop-in points (`docs/ARCHITECTURE.md §10`), not current
product surfaces or dead code.
See `docs/ARCHITECTURE.md §11` for the full disposition map.

### Go module

- Module path: `github.com/goobers/goobers`
- Minimum Go version: the toolchain declared in [`go.mod`](go.mod)

Import shared packages as e.g. `github.com/goobers/goobers/internal/version`.

### Binaries (`cmd/`)

The product binary is **`goobers`** — the local runner: `init`, `validate`, `up`
(daemon: scheduler + runner), `run`, `status`, `trace`.

Pre-existing entrypoints (`operator`, `scheduler`, `goober-runtime`) are
cloud-scale or superseded skeletons kept per the quarantine plan
(`docs/ARCHITECTURE.md §11`).
Every binary shares `internal/app.Main`, which wires `--version`, structured logging
(`--log-level`, `--log-format`), and SIGINT/SIGTERM-aware shutdown.

## Detailed local quickstart

New to declarative control systems? Read
[How Goobers works](docs/concepts/README.md) first; it explains why `config/`
defines behavior while runs, workcopies, and scheduler records are runtime
state.

No tagged release exists yet, so build from source with the Go toolchain
declared in [`go.mod`](go.mod). The fastest first run is the hermetic demo:

```sh
make build   # or: go build -o bin/goobers ./cmd/goobers

bin/goobers init --demo ./demo-instance
bin/goobers run demo ./demo-instance
bin/goobers trace <run-id> ./demo-instance
```

The demo runs the full curate -> implement -> review -> merge-preview loop on
Linux or macOS with mock providers, no credentials, and no network writes. It
also shows how a run pauses at a gate before completing.

From there, graduate in this order:

1. Seed the disposable, token-bearing `quickstart@v1` path with
   `bin/goobers init --template=quickstart ./tutorial-instance`.
2. Scaffold a regular instance with `bin/goobers init ./my-instance` and run
   its starter workflow with
   `bin/goobers run default-implement ./my-instance`.
3. Adapt the production-oriented definitions under [`config-examples/`](config-examples/),
   which add review, CI, remediation, escalation, and merge-policy patterns.

The [full quickstart](docs/guides/quickstart.md) walks through that progression
and the remaining CLI surfaces.

For a regular instance, the core inspection and operator commands are:

```sh
bin/goobers config show ./my-instance    # effective config (secrets redacted)
bin/goobers run default-implement ./my-instance # trigger a run manually
bin/goobers status ./my-instance         # list runs + their phase
bin/goobers claims list ./my-instance    # inspect current claim leases
bin/goobers claims release --force <item-id> ./my-instance # override a live holder
# If an item ID is claimed in multiple namespaces, add:
#   --gaggle=<name> --provider=<name>
bin/goobers trace <run-id> ./my-instance # inspect one run's journal
bin/goobers escalations ./my-instance    # list escalated runs
bin/goobers escalations show <run-id> ./my-instance # inspect cause + artifacts
```

Once a tagged release exists, a checksummed installer script will let you
install an exact release without a source checkout; see
[Releases & packaging](docs/guides/releases.md#install-a-pinned-release) for
that (currently unreleased) path and its reproducibility details.
`goobers init --guided` is a first-run flow that separately selects the
configuration source and target application repository. It creates or reuses a
checked-in source tree (`instance.yaml.example`, `manifest.yaml`, and
`gaggles/`), then materializes the runtime instance described in
`docs/ARCHITECTURE.md §6` — `instance.yaml`, `config/`, `runs/`, `scheduler/`,
`workcopies/`, and `telemetry.db`. The flow can also detect local coding-agent
harnesses and, after an explicit harness and destination preview, install the
release-matched agent toolkit only in the selected config source. Skipping that
step writes no toolkit files. Populated source and instance destinations are
never overwritten. Set the referenced credential environment variables at runtime
and author the workforce in the selected configuration source; the instance
records that source in `workflowSource` while runtime state remains outside it.
After later source edits, stop the daemon and run
`goobers config materialize ./my-instance` before restarting; this validates
and reapplies the recorded desired state without touching runtime state. The
`quickstart@v1` template is intentionally limited to a
disposable first success and omits production remediation, escalation, CI, and
merge policy.
`goobers up` runs the daemon (embedded scheduler + local runner): it restarts
any run interrupted by a prior crash via `Runner.Resume`, then drives
scheduled workflows until interrupted, draining in-flight runs gracefully on
SIGINT/SIGTERM. `run` remains the way to trigger one workflow manually
without a daemon running. Platform-specific setup:
[Linux quickstart](docs/guides/quickstart-linux.md),
[Windows quickstart](docs/guides/quickstart-windows.md); run
the daemon as a supervised service via
[Daemon supervision](docs/guides/supervision.md)
(systemd · launchd · Windows Service). How binaries are built, packaged, and
verified for distribution: [Releases & packaging](docs/guides/releases.md).
Before making the daemon API reachable beyond loopback, follow the
[OIDC authentication guide](docs/guides/oidc-authentication.md).
Azure DevOps instances can use
[Azure CLI, workload identity, managed identity, or PAT authentication](docs/guides/ado-authentication.md).
Operational workflows can query ADX or compiled organization adapters through
[external telemetry connectors](docs/guides/external-telemetry-connectors.md).

## Repository assistance with an agent

Each release publishes a portable
[Goobers agent toolkit](agent-toolkit/README.md) for an external coding agent
working in a config repository. Its canonical Agent Skills cover environment
resolution, [DSL authoring](skills/goobers-dsl-author/SKILL.md), read-only run
inspection, and workflow upgrades. Release-matched docs, schemas, examples, and
thin Copilot, Claude, and `AGENTS.md` adapters let it work without a source
checkout or running daemon. See the
[installation and usage guide](docs/guides/dsl-authoring-skill.md).

## Shell completion

Enable subcommand and flag completion, plus instance-aware workflow and recent
run ID completion, with the line for your shell (add it to the shell's startup
file to make it permanent):

```sh
source <(goobers completion bash)  # bash
source <(goobers completion zsh)   # zsh
goobers completion fish | source  # fish
goobers completion powershell | Out-String | Invoke-Expression  # PowerShell
```

## Developing

```sh
make verify-fast # pre-push format, vet, and Go build tier
make tidy-check  # check that go.mod/go.sum match tidy output
make ci          # merge gate (Go + config + portal)
make verify-full # merge plus integration, platform, and coverage gates
make vulncheck   # scan reachable Go code for known vulnerabilities
```

Portal builders can append `?portal-diagnostics=1` before the hash route (for
example, `http://localhost:5173/?portal-diagnostics=1#/runs`) to enable an
ephemeral `console.debug` stream with daemon request timing/status and the
initiating page, overlapping-request burst counts and elapsed time, and SSE
connect/disconnect/reconnect causes. This is default-off development tooling
for diagnosing the portal itself, not partner-facing workflow telemetry, and
it does not persist or send the events anywhere.

CI runs the same merge-tier implementation on every PR to `main`. See the
[`validation tier contract`](CONTRIBUTING.md#validation-tier-contract) for
audience guidance, CI job mapping, and per-platform prerequisites.

## Contributing

Goobers is open source and contributions are welcome. See
[`CONTRIBUTING.md`](CONTRIBUTING.md) for the workflow, [`SECURITY.md`](SECURITY.md)
for vulnerability disclosure, and the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

Licensed under the [MIT License](LICENSE).
