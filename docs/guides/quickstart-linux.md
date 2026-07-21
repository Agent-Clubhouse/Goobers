# Linux quickstart (tier 1, local)

Stand up the `goobers` daemon on a Linux host from scratch: install prerequisites,
build, configure credentials, and drive a first run. This is the Linux-specific
companion to the platform-neutral [`quickstart.md`](quickstart.md) — the CLI
surface is identical; this page calls out the few things that are Linux-specific
and records the exact environment Goobers is validated on.

Linux is a first-class node platform: the control plane has **no macOS coupling**
(no launchd/keychain/fsevents/hardcoded paths in Go source), and the daemon plus
the full implementation-workflow run green on Linux in CI on every change (see
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
| Go toolchain | the version pinned in [`go.mod`](../../go.mod) (currently **1.26**) |
| Git | `git worktree add`/`remove` are the only requirements → **git ≥ 2.17** |

The job records the exact kernel, distro `PRETTY_NAME`, and git/Go versions of
the run into a `linux-validation-evidence` artifact (`environment.txt` +
`summary.md` + the demo run's journal), so "supported on Linux" always has a
concrete, current referent. To reproduce locally on any POSIX host:

```sh
go build -o bin/goobers ./cmd/goobers
go run ./test/linuxvalidate -bin bin/goobers -out ./linux-validation-evidence
cat ./linux-validation-evidence/summary.md
```

## 1. Install prerequisites

```sh
# Go — install the toolchain matching go.mod (1.26). Distro packages often lag;
# prefer the official tarball:
curl -sSfL https://go.dev/dl/go1.26.0.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
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

To try the whole flow with **no repo or credentials**, use the offline demo —
the same fixture the CI validation drives:

```sh
goobers init --demo ./demo-instance
goobers run demo ./demo-instance                # deterministic triage → build → verdict
goobers trace <run-id> ./demo-instance
```

## 5. Run the daemon

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

## 6. Supervise it (systemd)

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
