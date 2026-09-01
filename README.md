<h1><img src="https://raw.githubusercontent.com/Agent-Clubhouse/Goobers/main/portal/public/goober-mascot-fallback.webp" alt="Purple Goober mascot" width="72"> Goobers</h1>

[![Website](https://img.shields.io/badge/website-goobers.dev-6d28d9)](https://goobers.dev)
[![CI](https://github.com/Agent-Clubhouse/Goobers/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Agent-Clubhouse/Goobers/actions/workflows/ci.yml)
[![Vulnerability Scan](https://github.com/Agent-Clubhouse/Goobers/actions/workflows/vulnerability-scan.yml/badge.svg)](https://github.com/Agent-Clubhouse/Goobers/actions/workflows/vulnerability-scan.yml)
[![Release](https://github.com/Agent-Clubhouse/Goobers/actions/workflows/release.yml/badge.svg)](https://github.com/Agent-Clubhouse/Goobers/actions/workflows/release.yml)
[![Latest release](https://img.shields.io/github/v/release/Agent-Clubhouse/Goobers)](https://github.com/Agent-Clubhouse/Goobers/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/Agent-Clubhouse/Goobers)](go.mod)

**Goobers is an open, self-hosted platform for running an AI workforce against
your repositories and backlog.** Instead of giving one agent an open-ended
prompt, you define a team of roles (a *gaggle*) and the workflow, permissions,
checks, retry limits, and human handoffs that govern its work.

## Contents

- [What is Goobers?](#what-is-goobers)
- [How it works](#how-it-works)
- [Install](#install)
- [Quick start](#quick-start)
- [Documentation](#documentation)
- [Shell completion](#shell-completion)
- [Development and contributing](#development-and-contributing)

## What is Goobers?

Goobers is for a solo builder who wants dependable issue-to-PR automation, a
team that wants agents to work within its existing review and CI policy, or an
organization that wants the same workforce definitions to remain useful as its
execution infrastructure grows.

The shipped runner is one Go binary that runs on one machine. Cloud-scale
orchestration is an explicit design goal, not a current product claim. The
configuration and run contract are designed to stay constant as execution
scales, so the workforce does not need to be redefined for a different runtime.

The core concepts are:

- A **goober** is a role-specialized AI worker with declared instructions,
  skills, tools, and capabilities.
- A **workflow** is a validated state machine of deterministic and agentic
  stages, gates, retries, budgets, and human handoffs.
- A **gaggle** is an isolated workforce and its workflows, connected to one
  project and backlog.
- An **instance** hosts one or more gaggles and owns their shared runtime state,
  claims, journals, workspaces, and telemetry.

See [How Goobers works](docs/concepts/README.md) for the complete trust and
configuration model.

## How it works

A Goobers workflow declares what may happen next instead of asking an agent to
control the run. Deterministic stages query backlogs, claim work, run checks,
poll CI, and create pull requests. Agentic stages invoke a named goober under
explicitly granted capabilities. Gates can approve a transition, send work
through a bounded repair loop, pause for a human, or escalate.

This keeps the model useful without making it the control plane:

- a lease-based [claim ledger](internal/localscheduler/claim.go) prevents two
  runs from silently taking the same backlog item;
- each run pins its workflow definition and records an
  [append-only, content-digested journal](internal/journal/doc.go); and
- isolated worktrees, deterministic checks, review gates, and repository policy
  control whether an agent-authored change can advance.

The shipped
[`implementation` workflow](reference-workflows/gaggles/goobers/workflows/implementation.yaml)
shows the complete path: claim one approved issue, implement in an isolated
worktree, route review and local-CI failures through bounded repasses, then push
the branch and open a pull request.

### Configuration is GitOps

Goobers treats workforce definitions as desired state. Keep `manifest.yaml`,
gaggles, goobers, instructions, and workflows in a protected configuration
repository. An instance tracks a configured branch, validates new revisions,
and atomically reloads valid definitions. Invalid revisions are rejected while
the last-known-good configuration continues running.

Agents can propose configuration changes, but normal repository governance
decides whether those changes become active. See the
[architecture of record](docs/ARCHITECTURE.md) for the full design and current
deployment boundaries.

## Install

Install the latest stable release on Linux or macOS:

```sh
VERSION="$(curl -fsSL https://api.github.com/repos/Agent-Clubhouse/Goobers/releases/latest |
  awk -F '"' '/tag_name/ { print $4; exit }')"
/bin/sh -c "$(curl -fsSL "https://github.com/Agent-Clubhouse/Goobers/releases/download/${VERSION}/install.sh")" \
  -- "$VERSION"
```

The installer verifies the downloaded archive against the release checksum and
places `goobers` in `$HOME/.local/bin`. See
[Release installation and verification](docs/guides/releases.md) for
prerequisites, version pinning, install-directory overrides, pre-releases, and
the Windows path.

## Quick start

Tour the full workflow locally without credentials or network writes:

```sh
goobers init --demo ./demo-instance
goobers run demo ./demo-instance
```

When you are ready to configure an existing GitHub or Azure DevOps repository,
start the guided browser setup:

```sh
goobers init --guided
```

The guided flow inspects the repository, derives what it can, adapts the
canonical workflows, prepares required repository metadata, and validates the
resulting instance. It does not execute a workflow.

Follow the [quickstart tutorial](docs/guides/quickstart.md) for the complete
learning path. For manual or agent-assisted setup, use
[Onboard an arbitrary repository](docs/guides/arbitrary-repo-onboarding.md) or
the release-matched [Getting Started skill](skills/goobers-getting-started/SKILL.md).

## Documentation

| Goal | Start here |
| --- | --- |
| Understand the product model | [Concepts](docs/concepts/README.md) |
| Learn with a credential-free workflow | [Quickstart tutorial](docs/guides/quickstart.md) |
| Configure a real repository | [Arbitrary repository onboarding](docs/guides/arbitrary-repo-onboarding.md) |
| Install on a specific host | [Linux](docs/guides/quickstart-linux.md), [macOS](docs/guides/quickstart-macos.md), or [Windows](docs/guides/quickstart-windows.md) |
| Operate an instance | [Daemon supervision](docs/guides/supervision.md) |
| Author workflows and configuration | [Agent toolkit](agent-toolkit/README.md) and [DSL authoring](docs/guides/dsl-authoring-skill.md) |
| Look up commands | [CLI reference](docs/cli/README.md) |
| Understand implementation and deployment boundaries | [Architecture](docs/ARCHITECTURE.md) |
| Explore the website and animated introduction | [goobers.dev](https://goobers.dev) |

The [historical product vision](docs/VISION.md) and some design documents
describe future direction. The overview above describes behavior shipped in
this tree.

## Shell completion

```sh
source <(goobers completion bash)  # bash
source <(goobers completion zsh)   # zsh
goobers completion fish | source  # fish
goobers completion powershell | Out-String | Invoke-Expression  # PowerShell
```

## Development and contributing

See [DEVELOPMENT.md](DEVELOPMENT.md) for the repository layout, build and
validation commands, binaries, and portal development notes.

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
workflow, [SECURITY.md](SECURITY.md) for vulnerability disclosure, and the
[Code of Conduct](CODE_OF_CONDUCT.md).

Licensed under the [MIT License](LICENSE).
