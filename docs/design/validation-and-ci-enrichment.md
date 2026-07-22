# Validation & CI Enrichment — closing the false-green gaps

**Status:** Approved for backlog planning (PO directive, 2026-07-16). Backlog-only future
investment: items are not `goobers:approved` and not eligible for automated implementation
until promoted.

**Goal:** enrich the local validation loop and CI so that "green" reliably means "main is
healthy" — for humans and for the agent workforce whose merge decisions key off it.

---

## 1. Current state and the incident record

`make ci` = `fmt-check vet build test lint`; GitHub Actions runs exactly that, single job,
ubuntu-latest. Everything else is opt-in and **not** in CI: `test-e2e` (walking skeleton),
`test-envtest` (operator), `cover-check` (coverage gate), conformance assertions.

Live operation has produced a concrete catalog of ways green lied to us:

- **G1 — Environment-dependent false green.** Tests that shell out to external CLIs pass
  locally because the tool is on the dev machine's PATH, then fail in CI (or worse, pass in
  CI for the wrong reason). Shipped a red-CI approve on #270.
- **G2 — Stubbed-IO false green.** V0.5's `merge-review` was 100% broken while its unit
  tests passed, because tests stubbed stage IO and nothing ran the *wired* workflow
  (v07-ladder-remediation L1: numeric outputs silently dropped by `InputResultFile`).
- **G3 — Flake masking.** A single `make ci` pass hides ~20% flaky timing/goroutine tests;
  a scheduler flake shipped on #21 because gating ran the suite once.
- **G4 — Semantic sibling collision.** Two PRs, each green in isolation and MERGEABLE,
  merged into a non-compiling main twice (#377+#378; signature change + new caller in a
  different file = no textual conflict). Nothing builds "main + this PR + that PR."
- **G5 — Discarded validation output.** The daemon throws away validator warnings
  (`cmd/goobers/daemon.go:61` and `cmd/goobers/workflow.go:67` discard the `*Report`;
  only `validate.go` keeps it), and the shipped workflow once invoked a subcommand
  that didn't exist — nothing validated workflow definitions against the real CLI surface.

Each workstream below names the gap it closes.

## 2. Design principles

1. **One command, tiered.** Developers, agents, and CI run the same entrypoints:
   `make verify-fast` (pre-push, minutes) ⊂ `make ci` (the merge gate) ⊂
   `make verify-full` (nightly/stress). No gate exists that can't be reproduced locally
   with one command.
2. **Hermetic by default.** The unit-test tier must not be able to *reach* ambient tools or
   network; anything non-hermetic lives in a named tier that declares its dependencies.
3. **The wired thing is the tested thing.** Every shipped workflow/config runs through the
   real runner (fake harness) in CI — stubs never stand in for the composition root.
4. **Green must compose.** The merge gate builds the PR against *current* main (merge
   queue), not against the PR's stale base.

## 3. Workstreams (issue map)

### Tier and gate structure

- **CI1. Promote e2e + envtest + coverage into CI.** Walking-skeleton e2e, operator
  envtest, and `cover-check` become required CI jobs (parallel to unit job). Closes the
  "opt-in means never" gap.
- **CI2. Merge queue.** Adopt GitHub merge queue on main with `make ci` (+CI1 jobs) as the
  queue check, so every merge is validated against post-merge main. Directly closes **G4**
  — this is the standing fix for the sibling-collision incidents. Includes updating
  merge-review/auto-merge workflows (V0.5) to queue-aware semantics.
- **CI3. `verify-fast` / `verify-full` tiers.** Define the tiered entrypoints, document
  them as the contract for humans and for agent workflows' `local-ci` stage (which today
  runs `make ci` as a daemon subprocess and inherits daemon-PATH quirks).

### Hermeticity (closes G1)

- **H1. Hermetic unit tier.** Test wrapper that scrubs PATH to an allowlisted toolchain
  (go, git) and sets no-network guards (e.g. `GOPROXY=off` for tests, refuse external
  exec); any test needing more must be tagged into an integration tier. Acceptance: hiding
  the copilot CLI (and any non-allowlisted binary) changes nothing in the unit tier.
- **H2. Declared-dependency integration tier.** Integration tests declare required tools;
  the harness fails loudly (skip-forbidden in CI) when a declared tool is absent —
  "silently skipped" is treated as red.

#### Hermetic unit-tier contract

`make test` and the portable CI gate's unit step both delegate to
`go run ./test/hermetic`. The wrapper creates a temporary PATH containing only
links to this documented allowlist:

- all platforms: `go`, `git`, and the C compiler selected by `go env CC` (needed
  by the race detector);
- Linux: the compiler subprocess helpers `as` and `ld`;
- Unix: `sh`, optional `bash`, `cat`, `dirname`, `echo`, `false`, `head`,
  `mkdir`, `rm`, `sleep`, `tr`, `true`, `wc`, and `yes`;
- Windows: `cmd.exe` and `icacls`.

The wrapper sets that directory as the complete PATH for `go test`. It also
sets `GOPROXY=off`, `GOSUMDB=off`, `GOTOOLCHAIN=local`, `GOVCS=*:off`, and
`GOFLAGS=-mod=readonly`, preventing the Go toolchain from resolving missing
dependencies or toolchains over the network. CI and local unit runs therefore
have the same ambient-tool and Go-network boundary. The entrypoints apply those
network guards to the outer `go run` that compiles the wrapper as well.

A test that needs any other executable or network access is not a unit test:
place it in a file guarded by `//go:build integration` and run it in the
declared-dependency integration tier. The wrapper statically rejects literal
`os/exec.Command` and `CommandContext` calls to non-allowlisted PATH tools with
that guidance; its restricted PATH also blocks dynamically selected ambient
tools at runtime. There is no environment-variable escape hatch that disables
unit-tier hermeticity.

### Wired-composition coverage (closes G2, G5)

- **W1. Shipped-workflow contract tests.** Every definition under `selfhost/` (and shipped
  templates) executes end-to-end through the real local runner + fake harness in CI, with
  journal-only assertions: stages run, outputs thread (`InputsFrom` including non-string
  values), gates evaluate, escalation paths fire. The L1/L3 class of breakage becomes
  unshippable.
- **W2. CLI-surface validation.** Validation cross-checks every deterministic stage
  command / subcommand referenced by shipped config against the real `goobers` CLI
  (exists, flags parse). No more invoking subcommands that don't exist.
- **W3. Validator warnings surfaced + gated.** Stop discarding the validation `*Report`:
  warnings surface in `goobers up`/`validate` output (shared plumbing with milestone #12),
  and CI treats warnings on shipped config as failures.
- **W4. Conformance suite in CI.** The journal determinism/conformance assertions (#29,
  `journal.ConformanceView`; later the V2 dual-runner harness) run as a CI job — the seam
  V2 depends on stays continuously enforced.

### Flake discipline (closes G3)

- **F1. Stress job.** Nightly + on-demand (`/stress` label or dispatch) job running
  timing/goroutine-sensitive packages under `-race -count=20`; label-selected packages via
  build tags or a package list file. QA-gate lore ("single pass hides ~20% flakes")
  becomes automation.
- **F2. Flake ledger + quarantine policy.** Failures from F1 auto-file/refresh a flake
  issue with failure fingerprints; a documented quarantine mechanism (skip-with-issue-link,
  expiry date) so flakes are managed, not muted. Acceptance: zero anonymous retries — CI
  never blanket-reruns failed jobs.
- **F3. Test-timing budget.** Per-job duration tracking with a soft budget; the stress and
  unit tiers publish timing artifacts so slow-creep is visible before it hurts.

### Enrichment

- **E1. Static-analysis expansion.** `govulncheck`, `staticcheck`-tier linters in
  golangci config, deadcode sweep, `go mod tidy -diff` check. Each addition lands with the
  repo already clean (one PR per analyzer to keep main evergreen).
- **E2. Portal/Go contract test in CI.** The dashboard doc commits "wire representation
  tested against the TypeScript client" — wire that test (Go contract types ↔ TS client
  fixtures) into CI alongside the portal build/typecheck, which is currently absent.
- **E3. Provider contract drift job.** The httptest-based provider contract tests get a
  scheduled variant against recorded/refreshed GitHub API fixtures, so upstream API drift
  surfaces as a scheduled red, not a live-run mystery.
- **E4. Large-repo benchmark harness in CI (reporting-only).** The V2 provisioning
  benchmark (v2-cloud-scale.md B0) runs scheduled with trend artifacts; not a gate until
  baselines stabilize.
- **E5. CI observability.** JUnit/artifact upload, failure annotations, and a one-page
  "why is main red" runbook; agent workflows (merge-review) consume the structured result
  instead of scraping logs.

## 4. Sequencing

1. **CI1 → CI2** first (they close the two incidents that actually broke main) — CI1 keeps
   each new job green-at-introduction by fixing any latent red before making it required.
2. **H1/W1** next (the two false-green classes with recorded incidents).
3. F-track and E-track are independent, one-PR-each, land in any order.
4. Cross-milestone: P6 (platform CI matrix) from the cross-platform milestone composes
   with CI1's job structure; E4 composes with V2's B0.

## 5. Out of scope

- Cloud/multi-node CI infrastructure (V2 concern).
- Replacing GitHub Actions.
- Coverage-percentage raising campaigns (the gate stays; raising thresholds is a
  by-product of W1, not a goal).
