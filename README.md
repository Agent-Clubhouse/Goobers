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

## Install

Install the current `v0.1.0` release on Linux or macOS:

```sh
/bin/sh -c "$(curl -fsSL https://github.com/Agent-Clubhouse/Goobers/releases/download/v0.1.0/install.sh)" \
  -- v0.1.0
```

The installer verifies the downloaded archive against the release checksum and
places `goobers` in `$HOME/.local/bin`. See
[Release installation and verification](docs/guides/releases.md) for
prerequisites, install-directory overrides, and the Windows path.

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

Follow the [canonical quickstart](docs/guides/quickstart.md) for the ordered
first-run path: a credential-free local demo, a disposable GitHub-backed run,
and then a regular instance using the
[production-oriented configuration examples](config-examples/README.md).

Prefer a guided walkthrough over typing CLI commands yourself? `goobers
getting-started` serves a portal-hosted alternative covering the same
first-run-against-your-own-repository ground — see
[the CLI reference](docs/cli/README.md#goobers-getting-started).

For deeper context, read the
[historical product vision snapshot (v0.3, July 2026)](docs/VISION.md),
[architecture of record](docs/ARCHITECTURE.md), [concepts](docs/concepts/),
and [requirements](docs/requirements/). Those documents include future
design; this overview deliberately describes the behavior shipped in this
tree.

## EvalSuite (workflow evaluation)

EvalSuite is an in-progress epic ([#2662](https://github.com/Agent-Clubhouse/Goobers/issues/2662))
for deterministic, reproducible evaluation of agentic workflows: side-by-side
(A/B) comparisons and shadow/dark runs against production-like input without
side effects. It is not yet shipped behavior — treat it the same as the other
future-design docs above until its child issues land.

- [Design overview and status](docs/design/evals-suite.md)
- [Onboarding: running EvalSuite tests and reading reports](docs/guides/evals-onboarding.md)
- [PR review checklist for EvalSuite artifacts](docs/guides/evals-review-checklist.md)

## Engineering reference

### Repository layout

| Path | Contents | Status |
|---|---|---|
| `api/` | Definition types, JSON invocation/result/verdict envelopes, YAML schema | Active |
| `config/` | CRD manifests for tier-3 config delivery | **Quarantined** — reserved for cloud-scale execution |
| `providers/` | Backlog + repo provider abstraction (GitHub / ADO) | Active |
| `cmd/goobers` | The product binary: `init`, `validate`, `up`, `run`, `status`, `trace` | Active |
| `cmd/operator` | Kubernetes operator entrypoint | **Quarantined** — reserved for cloud-scale execution |
| `internal/operator` | Kubernetes operator reconcile logic | **Quarantined** — reserved for cloud-scale execution |
| `internal/configsync` | Config-repo → CRD render/apply (ArgoCD bridge) | **Quarantined** — CRD-apply path is not part of the shipped local runner |
| `internal/` (other) | Shared Go packages (engine core, telemetry, app bootstrap) | Active |
| `infra/` | Bicep, ArgoCD, Temporal, ADX | **Quarantined** — cloud infrastructure is not a shipped deployment path |
| `portal/` | TypeScript + React observability portal; operator co-brandable via `instance.yaml` | Active |
| `reference-workflows/` | Canonical, proven workflow configs used to operate Goobers against this repository | Active — tested dogfood reference |
| `config-examples/` | Reference config layout + starter definitions | Active |
| `examples/` | Specialized end-to-end examples, including iOS simulator workflows | Active |
| `samples/` | Versioned, disposable onboarding targets | Active |
| `deploy/` | Customer-applied Kubernetes reference manifests for the cloud-scale deployment shape | Reference — not a managed deployment path |
| `telemetryconnector/` | Versioned extension API for external operational telemetry connectors | Active |
| `evals/` | EvalSuite design docs and prototype runner/adapter implementation for deterministic, reproducible workflow evaluation | Prototype runner in-tree — not shipped behavior |
| `release/` | Release archive, installer, notes, metadata, and onboarding packaging tools | Active |
| `packaging/` | Container build and embedded service-supervisor assets | Active |
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

Pre-existing entrypoints (`operator`, `config-sync`) are cloud-scale skeletons
kept per the quarantine plan (`docs/ARCHITECTURE.md §11`). The tier-3
scheduler fork (`cmd/scheduler`, `internal/scheduler`) and `cmd/goober-runtime`
were deleted per `docs/design/goobernetes-architecture.md` D5 (#2055 resolved:
supersede).
Every binary shares `internal/app.Main`, which wires `--version`, structured logging
(`--log-level`, `--log-format`), and SIGINT/SIGTERM-aware shutdown.

## Onboarding and operation

The [canonical quickstart](docs/guides/quickstart.md) owns the complete,
ordered first-run flow and CLI walkthrough. Use these focused guides only for
the differences relevant to your environment:

- [Linux host setup](docs/guides/quickstart-linux.md)
- [macOS host setup](docs/guides/quickstart-macos.md)
- [Windows host setup](docs/guides/quickstart-windows.md)
- [Release installation and verification](docs/guides/releases.md)
- [Onboard an arbitrary repository (tiers 1-2)](docs/guides/arbitrary-repo-onboarding.md)
- [Custom deterministic stage cookbook](docs/guides/custom-stage-cookbook.md)
- [Daemon supervision](docs/guides/supervision.md)
- [OIDC authentication](docs/guides/oidc-authentication.md)
- [Azure DevOps authentication](docs/guides/ado-authentication.md)
- [External telemetry connectors](docs/guides/external-telemetry-connectors.md)

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
