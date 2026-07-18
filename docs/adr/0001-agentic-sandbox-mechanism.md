# ADR 0001: Use OS-native sandboxes for local agentic stages

- Status: Accepted
- Date: 2026-07-18
- Decision owner: SEC-Q6 / issue #163

## Context

V1 requires agentic subprocesses to be sandboxable on developer workstations and
small-team hosts (SEC-044). The GitHub Copilot CLI must retain access to its local
authentication and platform runtime while writes outside the stage worktree are
denied. The mechanism must work on macOS and Linux without changing the harness
contract.

The spike compared three options:

| Option | Finding |
|---|---|
| OS-native | Seatbelt (`sandbox-exec`) on macOS and bubblewrap (`bwrap`) on Linux can make the host filesystem read-only and mount only the worktree writable. The locally installed CLI and its authentication remain available. |
| Container | Provides a stronger, more uniform boundary, but requires a daemon/image lifecycle and explicit forwarding of the host Copilot authentication and keychain. That is too heavy for the tier-1 single-binary deployment. |
| Harness-native | Copilot tool permissions govern tool approval, not an independently enforceable filesystem boundary. A compromised or incorrect harness cannot be constrained by its own policy. |

## Decision

Use an OS-native sandbox for V1 local agentic execution:

- macOS wraps the harness with Seatbelt. The prototype starts from normal host
  access, denies all filesystem writes, then permits writes only in the canonical
  worktree, terminal/null devices, and explicitly declared runtime-state roots.
- Linux wraps the harness with bubblewrap. The prototype mounts the host root
  read-only, provides private `/dev` and `/proc`, and bind-mounts only the
  canonical worktree plus explicitly declared runtime-state roots read-write.
- `internal/sandbox.Sandbox` is the platform-neutral seam. It rewrites an
  `exec.Cmd` while leaving environment construction, stdio, timeouts, and process
  groups with the harness runner.
- Absence of the native mechanism is an explicit error. S3 must fail closed by
  default and implement any trusted-local opt-out above this seam.

Container isolation is deferred to tier 3, where ephemeral pods are already the
execution substrate. Harness-native permissions remain defense in depth, not the
sandbox boundary.

## Evidence

`TestNativeSandboxConfinement` runs a scripted child in a temporary worktree,
proves an in-worktree write succeeds, and proves an out-of-worktree write is
denied. CI runs this probe with bubblewrap on Linux and Seatbelt on macOS.

Copilot CLI 1.0.71 does not expose a session-state directory override and
persists resumable events beneath `~/.copilot/session-state`; `--log-dir` does
allow logs to be redirected into the worktree. On 2026-07-18, the opt-in live probe
`GOOBERS_SANDBOX_COPILOT_LIVE=1 go test ./internal/sandbox -run
TestNativeSandboxCopilotLive -count=1` succeeded on macOS with GitHub Copilot CLI
1.0.71: `copilot -p` authenticated, executed non-interactively under Seatbelt,
and created its requested file inside the temporary worktree while Seatbelt
allowed only that worktree and the CLI's session-state directory to be written.

## Consequences and residual risks

- Seatbelt is deprecated and undocumented but remains present and functional on
  supported macOS releases. Startup preflight must treat its removal as sandbox
  unavailability, never silently execute bare.
- Bubblewrap is an external Linux dependency and requires user namespaces or a
  correctly installed setuid helper. Availability must be checked before stage
  dispatch.
- This decision constrains filesystem mutation, not all reads. Runtime binaries,
  shared libraries, certificates, and local Copilot authentication remain
  readable. S2 should narrow read access where platform testing proves it does
  not break authentication.
- Copilot's session-state directory is a narrow out-of-worktree write exception
  until the CLI supports relocating it. S3 must provision a per-run state root
  when a supported override becomes available; until then, resumable session
  metadata is shared with the local user account and remains a residual risk.
- The prototype does not restrict network egress. Copilot requires network
  access; V1 documents that residual risk, while tier 3 enforces network policy.
- The wrapper deliberately avoids creating a new session so the harness
  runner's existing process-group timeout and kill semantics remain intact.
