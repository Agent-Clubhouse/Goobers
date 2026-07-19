# Cross-Platform Support — Linux & Windows nodes

**Status:** Approved for backlog planning (PO directive, 2026-07-16). Backlog-only future
investment: items are not `goobers:approved` and not eligible for automated implementation
until promoted.

**Goal:** Goobers nodes — the local daemon at tiers 1–2 and cloud workers/pods at tier 3 —
run first-class on **Linux and Windows** in addition to the current macOS development
reality. "First-class" means: documented install, green CI on the platform, daemon +
full implementation-workflow e2e verified, and platform-specific supervision.

---

## 1. Current state (2026-07-16 survey)

The good news: there is **less macOS coupling than assumed**.

- GitHub Actions CI already runs `make ci` on **ubuntu-latest** — the build/unit-test path
  is proven on Linux today. No launchd plists, no keychain calls, no fsevents, no
  hardcoded `/Users` paths exist in Go source. `runtime.GOOS` appears only in a version
  string.
- The release design (milestone #12) already targets **darwin/linux × arm64/amd64**
  binaries. **Windows appears nowhere in the doc set.**

What actually couples us to POSIX (breaks **Windows**; fine on Linux):

| Coupling | Sites | Windows problem |
|---|---|---|
| `syscall.Flock(LOCK_EX/LOCK_NB)` | `cmd/goobers/lock.go:20`, `cmd/goobers/providercmd.go:140`, `internal/journal/run.go:55` | No flock; needs `LockFileEx` |
| Process groups: `SysProcAttr{Setpgid}` + `syscall.Kill(-pid, SIGKILL)` | `internal/harness/process.go:190,225` | Needs Job Objects |
| Unix signals (SIGTERM; `Signal(0)` liveness probe) | `internal/signals/signals.go:25`, `internal/worktree/reap.go:274` | `os.Interrupt` only; no kill-0 |
| Token-file permission check wants 0600 | `internal/credentials/source.go:75` | Unix mode bits are fiction on NTFS; needs ACL check |
| bash: `SHELL := /usr/bin/env bash` and Bash recipes | Makefile | No bash on stock Windows |
| Agent sandboxing design is Seatbelt (macOS) / bubblewrap (Linux) only | docs/design/v1/35-… | No Windows mechanism named |

Also platform-sensitive though not broken: git worktree behavior on Windows (long paths,
`core.symlinks`, CRLF), daemon supervision (DEP-Q6 names only systemd/launchd), and the
Copilot CLI harness (must itself be installed/authenticated per platform).

## 2. Strategy

**Phase L (cheap, now-ish): make Linux officially supported, not incidentally working.**
Linux is already ~green; the work is validation, docs, supervision, and keeping it green.
Cloud nodes (tier-3 workers/pods) are Linux-first, so Phase L is also the tier-3
prerequisite.

**Phase W (the real port): Windows.** One **platform abstraction layer** for the four
POSIX couplings (lock, process-group, signals, secret-file permissions) with build-tagged
implementations — no `runtime.GOOS` branching scattered through call sites. Then bash
removal, CI matrix, worktree/git audit, supervision, release artifacts.

Ordering rule for evergreen main: each abstraction lands as a refactor PR that is
**behavior-identical on darwin/linux** (existing CI proves it), with the Windows
implementation as a follow-up PR gated by the Windows CI job — main never depends on a
half-ported platform.

## 3. Workstreams (issue map)

- **P1. Platform lock abstraction** — `internal/platform/lock`: flock (unix) /
  `LockFileEx` (windows). Ports the three flock sites. Includes cross-process contention
  tests.
- **P2. Process-tree control abstraction** — `internal/platform/proc`: process-group
  spawn+kill (unix) / Job Objects (windows); liveness probe (kill-0 / handle wait).
  Ports `harness/process.go` and `worktree/reap.go`.
- **P3. Signals & shutdown abstraction** — SIGTERM/SIGINT (unix) / `os.Interrupt` +
  service stop events (windows); one graceful-shutdown path.
- **P4. Secret-file protection portability** — 0600 check (unix) / owner-only ACL check
  (windows) in `credentials.TokenRef`; fail-closed on both.
- **P5. De-bash the toolchain** — coverage gate replaced by a Go tool
  (`go run ./test/coveragegate`); remaining Makefile bashisms audited or fronted by a
  portable task runner; contributor docs.
- **P6. CI platform matrix** — `make ci` on ubuntu + macos + windows (macOS joins the
  matrix too: today's CI never tests the primary dev platform). Windows job may start
  allow-failure while P1–P5 land, then becomes required. (Wiring detail shared with the
  Validation & CI milestone; the matrix itself is owned here.)
- **P7. Linux node validation + quickstart** — full daemon + implementation-workflow e2e
  on a Linux box (fake-harness + live-smoke variants); document quickstart deltas
  (golangci-lint on daemon PATH, Go 1.26 toolchain, git version floor).
- **P8. Daemon supervision units** (resolves DEP-Q6) — systemd unit (Linux), launchd plist
  (macOS), Windows Service wrapper; `goobers service install|uninstall|status` or
  documented unit files (lean: documented unit files first, subcommand later).
- **P9. Git/worktree Windows audit** — long-path support, `core.symlinks=false` behavior,
  CRLF/`.gitattributes` policy for managed working copies, path-length of
  `workcopies/<key>/runs/<runID>-<stage>`; fixture tests on the Windows CI job.
- **P10. Windows agent-harness reality check (spike)** — Copilot CLI install/auth/exec on
  Windows under the P2 process model; documents what works, what's degraded, and whether
  agentic stages on Windows nodes are supportable or deterministic-only at first.
- **P11. Windows execution isolation posture (design)** — the #35 sandbox ladder has no
  Windows rung; decide and document (AppContainer / restricted token / container-only /
  explicitly-unsandboxed-with-warning). Design-first; implementation follows the S0
  mechanism spike's fate.
- **P12. Release matrix + packaging** — add windows/amd64 (arm64 stretch) to the tagged
  release build (milestone #12's pipeline); checksum/signing story; scoop/winget as the
  Homebrew-tap analog (documentation-level initially).
- **P13. Mixed-platform cloud nodes (design)** — tier-3 note: Linux pods are the default
  execution substrate; Windows *worker nodes* matter only for teams whose build/test
  requires Windows — shape: Windows node pool + task-queue routing by platform label.
  Design doc + conformance implications; no implementation until a customer shape demands
  it.

## 4. Acceptance shape for the milestone

- CI matrix green (required) on ubuntu/macos/windows for `make ci`.
- Documented, supervised daemon install on all three platforms.
- Implementation-workflow e2e (fake harness) proven per platform; live-smoke documented
  where the agent harness supports the platform (P10 outcome).
- Sandboxing posture stated per platform (even where the statement is "none yet — trusted
  local only, logged").

## 5. Out of scope

- BSDs, musl/Alpine oddities beyond what ubuntu CI covers, ARM Windows.
- Porting the *portal* (browser-based; already platform-neutral).
- Windows-specific sandbox implementation (P11 decides posture only).
