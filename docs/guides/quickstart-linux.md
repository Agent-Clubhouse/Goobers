# Linux quickstart (tier 1, local)

Stand up the `goobers` daemon on a Linux host from scratch: install prerequisites,
build, configure credentials, and drive a first run. This is the Linux-specific
companion to the platform-neutral [`quickstart.md`](quickstart.md) — the CLI
surface is identical; this page calls out the few things that are Linux-specific
and records the exact environment Goobers is validated on.

Linux is a first-class node platform: the control plane has **no macOS coupling**
(no launchd/keychain/fsevents/hardcoded paths in Go source), and the daemon plus
the deterministic implementation-workflow path (with a fake agent harness) run
green on Linux in CI on every change (see
[Validated environment](#validated-environment)). Cloud tier-3 nodes are
Linux-first, so this is also the substrate that path builds on.

## Validated environment

Goobers' Linux support is exercised on every PR by the `linux node validation`
CI job (`.github/workflows/ci.yml`), which runs the shipped binary end to end —
an offline demo run to completion and the full daemon start/status/stop
lifecycle under a real `SIGTERM` — on the GitHub-hosted `ubuntu-latest` runner.

| Component | Validated on |
|---|---|
| Distribution | Ubuntu 24.04 LTS (`ubuntu-latest`) |
| Architecture | linux/amd64 |
| Go toolchain | the version pinned in [`go.mod`](../../go.mod) (currently **1.26.5**) |
| Git | `git worktree add`/`remove` are the only requirements → **git ≥ 2.17** |

A representative captured run (from the CI evidence artifact): Ubuntu 24.04.4
LTS, kernel 6.17, git 2.54.0, Go 1.26.0, linux/amd64 — demo run
`phase=completed`, daemon lifecycle clean. The job records the exact kernel,
distro `PRETTY_NAME`, and git/Go versions of *each* run into a
`linux-validation-evidence` artifact (`environment.txt` + `summary.md` + the
demo run's journal), so "supported on Linux" always has a concrete, current
referent.

> **Linux delta — deterministic `network: none` stages use user namespaces.** On
> Linux, a workflow stage that declares `network: none` is isolated with an
> unprivileged user + network namespace (`CLONE_NEWUSER`), not an external
> sandbox. This works out of the box on the validated Ubuntu 24.04 runner. Some
> hardened distros disable unprivileged user namespaces (e.g. a non-default
> `kernel.apparmor_restrict_unprivileged_userns=1` or
> `kernel.unprivileged_userns_clone=0`); if a deterministic stage fails to fork
> there, enable unprivileged user namespaces for the daemon's user. To reproduce locally on any POSIX host:

```sh
go build -o bin/goobers ./cmd/goobers
go run ./test/linuxvalidate -bin bin/goobers -out ./linux-validation-evidence
cat ./linux-validation-evidence/summary.md
```

## 1. Install prerequisites

```sh
# Go — install the toolchain matching go.mod (1.26.5). Distro packages often lag;
# prefer the official tarball:
curl -sSfL https://go.dev/dl/go1.26.5.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
export PATH="/usr/local/go/bin:$(go env GOPATH)/bin:$PATH"

# Git (>= 2.17 — any supported Ubuntu/Debian is newer):
sudo apt-get update && sudo apt-get install --yes git

# golangci-lint — REQUIRED on the daemon's PATH (see the note in step 5):
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.12.2/install.sh \
  | sh -s -- -b "$(go env GOPATH)/bin" v2.12.2
```

> Node.js 24 + npm are only needed to build/test the **portal** or run the full
> `go run ./test/ci` gate — not to run the daemon. See
> [CONTRIBUTING.md](../../CONTRIBUTING.md#platform-prerequisites) for the dev gate.

## 2. Build the binary

```sh
go build -o bin/goobers ./cmd/goobers    # or: make build
sudo install -m 0755 bin/goobers /usr/local/bin/goobers   # optional: put it on PATH
```

## 3. Scaffold and configure an instance

```sh
goobers init ./my-instance
```

Edit `my-instance/instance.yaml` to point at your repo and reference a provider
token. **Never inline the secret** — reference an env var or a file (CFG-009 /
SEC-010). If you use a token file, lock its permissions down; Goobers fail-closes
on a world- or group-readable token file:

```sh
mkdir -p ~/.config/goobers
printf '%s' "$GITHUB_TOKEN" > ~/.config/goobers/github.token
chmod 600 ~/.config/goobers/github.token      # 0600 required — Goobers rejects looser modes
```

Then in `instance.yaml`, set the repo's token ref to `env: GOOBERS_GITHUB_TOKEN`
(and export it before `up`) or `file: ~/.config/goobers/github.token`. Validate:

```sh
goobers validate ./my-instance
```

## 4. First run

Trigger one workflow manually (no daemon required):

```sh
goobers run <workflow-name> ./my-instance
goobers status ./my-instance                    # list runs + phase
goobers trace <run-id> ./my-instance            # inspect the run journal
```

To watch the whole loop with **no repo, provider credentials, model tokens, or
network writes**, use the hermetic mock-provider demo — the same fixture the
Linux validation drives:

```sh
goobers init --demo ./demo-instance
goobers run demo ./demo-instance                # curate -> implement -> review -> merge preview
goobers trace <run-id> ./demo-instance
```

## 5. Operator-run Linux live-smoke (real Copilot CLI)

This optional, manual check exercises the boundary that the hermetic Linux CI
job cannot: a real Copilot CLI process authenticating and running the agentic
stages of one `implementation` workflow. It mutates the configured target
repository by claiming an issue, pushing a branch, opening a PR, and updating
the issue. Use a provisioned test host and a disposable issue/repository where
those changes are expected.

### Prerequisites

| Component | Live-smoke requirement |
|---|---|
| Linux | Prefer the validated Ubuntu 24.04 LTS, linux/amd64 baseline. On another distribution, record the distribution and kernel and ensure unprivileged user namespaces are available for `network: none` stages. |
| Git | Git 2.17 or newer, with `git worktree add` and `git worktree remove`. |
| Go | The version pinned in [`go.mod`](../../go.mod) (currently 1.26.5), on the PATH inherited by Goobers. The target repository's build/test tools, including `golangci-lint` for the Goobers self-host workflow, must be on that PATH too. |
| Copilot CLI | A current stable GitHub Copilot CLI on the same user's PATH, an active Copilot entitlement, and an organization policy that permits Copilot CLI. Record `copilot --version`. |
| Goobers instance | A validated instance containing the `implementation` workflow, repository capability credentials, working local and hosted CI, and exactly one dedicated issue eligible under that workflow's trust/readiness labels. The shipped workflow expects `goobers:approved` and `goobers:ready`; open-PR caps and run budgets must also admit the run. |

Install Copilot CLI with an official method. For example, the official Linux
installer installs under `$HOME/.local` for a non-root user:

```sh
curl -fsSL https://gh.io/copilot-install | bash
export PATH="$HOME/.local/bin:$PATH"
copilot --version
```

See GitHub's
[Copilot CLI installation](https://docs.github.com/copilot/how-tos/copilot-cli/set-up-copilot-cli/install-copilot-cli)
instructions for other supported methods. For an interactive host, authenticate
as the same OS user that runs Goobers:

```sh
copilot login
```

On Linux, the stored OAuth session uses libsecret when a keyring is available
and otherwise may use `~/.copilot/config.json`; a systemd service must run with
the same home/profile or it will not see that session. For a headless service,
configure a separate personal fine-grained PAT with **Copilot Requests:
Read-only** as the instance's `agent:model` credential instead of relying on a
stored login. Keep it separate from repository credentials as described in
[GitHub token scopes](github-token-scopes.md#agentic-copilot-harness-stages-stored-login-or-agentmodel-token).
Never put either token in this evidence bundle.

### Run and capture evidence

The commands below are literal except for the one marked value:
`/absolute/path/to/provisioned-instance`. Replace that value before running the
block. `RUN_ID` and the timestamped evidence directory are derived
automatically.

```bash
set -u -o pipefail

# REQUIRED PLACEHOLDER: replace this value with the existing instance root.
export GOOBERS_INSTANCE="/absolute/path/to/provisioned-instance"
export EVIDENCE_DIR="$HOME/goobers-linux-live-smoke-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$EVIDENCE_DIR"

{
  printf '%s\n' "captured_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  uname -a
  sed -n '1,20p' /etc/os-release
  git --version
  go version
  goobers --version
  copilot --version
} 2>&1 | tee "$EVIDENCE_DIR/environment.txt"

goobers validate --check-harness "$GOOBERS_INSTANCE" \
  2>&1 | tee "$EVIDENCE_DIR/preflight.txt"
PREFLIGHT_EXIT="${PIPESTATUS[0]}"
if [ "$PREFLIGHT_EXIT" -ne 0 ]; then
  printf '%s\n' "Harness preflight failed; retain the evidence directory and stop."
  exit "$PREFLIGHT_EXIT"
fi

goobers run implementation "$GOOBERS_INSTANCE" \
  2>&1 | tee "$EVIDENCE_DIR/run.txt"
RUN_EXIT="${PIPESTATUS[0]}"
printf 'goobers_run_exit=%s\n' "$RUN_EXIT" \
  | tee "$EVIDENCE_DIR/run-exit.txt"

RUN_ID="$(sed -n 's/^created run \([^ ]*\).*/\1/p' \
  "$EVIDENCE_DIR/run.txt" | tail -n 1)"
if [ -z "$RUN_ID" ]; then
  printf '%s\n' "No run ID found; retain the evidence directory and stop."
  exit 1
fi
printf '%s\n' "$RUN_ID" | tee "$EVIDENCE_DIR/run-id.txt"

goobers status --workflow implementation --limit 10 "$GOOBERS_INSTANCE" \
  2>&1 | tee "$EVIDENCE_DIR/status.txt"
goobers trace "$RUN_ID" "$GOOBERS_INSTANCE" \
  2>&1 | tee "$EVIDENCE_DIR/trace.txt"
goobers trace --json "$RUN_ID" "$GOOBERS_INSTANCE" \
  > "$EVIDENCE_DIR/trace.json"
goobers trace --transcripts "$RUN_ID" "$GOOBERS_INSTANCE" \
  > "$EVIDENCE_DIR/transcripts.txt"

RUN_DIR="$(find "$GOOBERS_INSTANCE/gaggles" -type d \
  -path "*/runs/$RUN_ID" -print -quit)"
printf '%s\n' "$RUN_DIR" | tee "$EVIDENCE_DIR/run-journal-path.txt"
if [ -n "$RUN_DIR" ]; then
  tar -C "$(dirname "$RUN_DIR")" -czf "$EVIDENCE_DIR/run-journal.tgz" "$RUN_ID"
fi
```

`goobers run` waits for a terminal phase. A successful live-smoke prints
`finished: phase=completed` and exits 0. Its trace must show a claimed item and
the real agentic `implement` and reviewer attempts, followed by local CI, branch
push, PR open, hosted-CI poll, issue close-out, and `run.finished` with
`status=completed`. Retain:

- `environment.txt`, `preflight.txt`, `run.txt`, `run-exit.txt`, and
  `status.txt`;
- the human and JSON traces plus `transcripts.txt`;
- `run-id.txt`, `run-journal-path.txt`, and the archived run journal; and
- the resulting issue and PR URLs.

A `completed` run whose query reports `no-work` is valid runner behavior but
**does not count** as this live-smoke because no Copilot-backed stage ran.
Likewise, `failed`, `aborted`, or `escalated` is terminal but is not the
expected result. Use the last error or failed stage in `trace.txt` as the
failure boundary. Transcripts can contain repository context even though the
journal writer scrubs secret-shaped values; inspect and redact the bundle
before sharing it.

### Linux failure notes and reporting

- `copilot: command not found` during preflight or an agentic stage means the
  Goobers process cannot see the CLI. Fix the PATH of the actual daemon or
  systemd user, not only the interactive shell.
- Authentication that works interactively but fails under systemd usually
  means the service has a different `HOME`, cannot access the user's keyring,
  or needs an explicit `agent:model` credential. A personal fine-grained PAT
  needs **Copilot Requests: Read-only**; classic PATs are not supported.
- `operation not permitted` while starting a deterministic `network: none`
  stage usually means the distribution disabled unprivileged user namespaces.
  Apply the host policy described in [Validated environment](#validated-environment)
  or report that policy as the blocker.
- Token-file validation failures require owner-only mode (`chmod 600`).
  Permission/ownership failures under `gaggles/*/workcopies` should be reported
  with the service user and paths, never by making credentials or the whole
  instance world-readable.

Record the provisioned-host exercise and its redacted evidence on
[#1472](https://github.com/Agent-Clubhouse/Goobers/issues/1472). For a product
failure, include the run ID, failing stage/command, exit code, OS/kernel, Git,
Go, Goobers, and Copilot CLI versions, and the redacted trace; never include
tokens. Any future credentialed, opt-in hosted-runner version belongs in
[#1473](https://github.com/Agent-Clubhouse/Goobers/issues/1473), not in the
ordinary CI job.

The ordinary `linux node validation` job and fake-harness workflow tests remain
the deterministic regression signal for Linux runner, journal, worktree, and
daemon behavior. They intentionally carry no live Copilot credential and
therefore prove **neither** Copilot installation nor subscription, policy,
keyring/profile access, token scope, model reachability, or live
authentication. This operator run supplements that CI boundary; it does not
replace it.

## 6. Run the daemon

```sh
goobers up ./my-instance        # foreground; Ctrl-C (SIGINT) or SIGTERM to stop
```

`goobers up` runs the embedded scheduler + local runner in the foreground and
blocks until interrupted, draining in-flight runs gracefully on SIGINT/SIGTERM
(a second signal force-exits). Check health from another shell with
`goobers status --daemon ./my-instance`.

> **Linux delta — the daemon's PATH is not your shell's.** A workflow's
> `local-ci` stage runs `make ci`/`golangci-lint` as a *subprocess of the
> daemon*, inheriting the daemon process's environment, not your interactive
> dotfiles. Ensure `golangci-lint` and the Go toolchain are on the PATH the
> daemon sees. Under a systemd unit this is the unit's `Environment=PATH=…`
> (see supervision, below); when launched from a shell it is that shell's PATH.

## 7. Supervise it (systemd)

For an unattended node, run the daemon under **systemd** instead of a foreground
shell. A ready-to-edit user-service template and full install/start/stop/status/
logs/upgrade instructions are in
[Daemon supervision](supervision.md#linux-systemd) — including the template at
[`packaging/systemd/goobers.service`](../../packaging/systemd/goobers.service).

## Deltas from the macOS flow, at a glance

The CLI is byte-for-byte identical to macOS; only the surrounding host tooling
differs:

| Aspect | macOS | Linux |
|---|---|---|
| Tool install | Homebrew (`brew install`) | distro packages / official tarballs (`apt-get`, `go.dev` tarball) |
| Supervision | launchd LaunchAgent | systemd user service |
| Daemon-PATH caveat | identical — the `local-ci` stage inherits the daemon's PATH on both | identical |
| 0600 token-file check | enforced | enforced |

Everything else — `init`, `validate`, `run`, `up`, `status`, `trace`,
signal-driven graceful shutdown — behaves the same. See
[`quickstart.md`](quickstart.md) for the full command-by-command walkthrough and
[V0-ACCEPTANCE.md](../V0-ACCEPTANCE.md) for the end-to-end acceptance runbook.
