# V0 Acceptance Runbook

> Status: **Scaffolded, not yet executed.** This is the verification artifact for
> issue #30, the V0 milestone gate. It is structured against the expected
> definition of done now, ahead of its remaining dependencies (#17, #19, #23,
> #24, #26, #28, #29) landing; commands marked **[pending]** below don't exist
> on `main` yet. The milestone closes only once someone other than the
> runner's primary implementer executes this runbook clean, end to end, and
> the [Execution record](#execution-record) appendix is filled in with real
> journal excerpts and PR links (issue #30's acceptance criteria) — not before.
> Owner: Goobers-Dev-5.

## Purpose

This runbook proves the V0 definition of done (`docs/ARCHITECTURE.md` §12,
"Works locally, begins to build itself"):

> A single machine runs a gaggle against a real GitHub repo (including this
> one) ... feed issues into the backlog and watch them get curated, scoped,
> and implemented into PRs by the instance running on your own machine.

It is a procedure, not a test suite: a human (or a future automated check)
follows the numbered steps on a real machine against a real GitHub repo and
either it works, cleanly, or it doesn't — and any gap becomes a filed bug
against the responsible issue, per issue #30's scope.

## Prerequisites

- Go 1.26+ (matches `go.mod`).
- A GitHub personal access token with `repo` scope for the target repo (see
  `docs/guides/github-token-scopes.md`) and, for a real dogfood run, write
  access to a repo you're willing to have the instance open PRs against —
  **use a scratch/fork repo for the first execution, not this one**, until
  the loop has been proven once.
- The [self-hosting dogfood config](#28) this repo ships (#28, in progress) —
  until it lands, substitute `config-examples/` (already on `main`) to
  exercise the mechanics, understanding that its single starter workflow
  doesn't demonstrate the full curation → nomination → implementation chain.

## Procedure

### 1. Setup

```sh
# Build the binary (or `go run ./cmd/goobers ...` throughout instead).
go build -o bin/goobers ./cmd/goobers

# Scaffold a fresh instance root.
./bin/goobers init ./my-instance
cd my-instance

# Point instance.yaml's connections at the target repo + a github-pat token
# ref (see instance.yaml's inline comments), then drop in the dogfood config
# (#28) — or config-examples/ as an interim stand-in — under config/.

# Validate before anything runs (fails closed on bad config/definitions).
../bin/goobers validate .
```

### 2. Run **[pending #17, #23]**

```sh
# goobers up / goobers run don't exist on main yet — #17 (runner core) is the
# execution engine, #23 wraps it as `up` (daemon: scheduler + runner) and
# `run` (one manual trigger, still honoring run conditions). Once landed:
../bin/goobers up          # long-lived: embedded scheduler + local runner
# or, for a single manual pass:
../bin/goobers run <workflow-name>
```

Seed the loop by filing N (start with 3–5) real issues against the target
repo — plain, small, well-scoped asks a coder goober could plausibly finish.
Backlog curation (#25, shipped) is the first stage of all three shipped
workflows; it should pick these up on its next scheduled or manual run.

### 3. Observe

Watch the three shipped workflows carry an issue from raw backlog item to an
open PR:

1. **Backlog curation** (#25, shipped) — dedupe/tag/split/mark-ready. Confirm
   in the provider (GitHub issue labels/comments) that seeded issues get
   curated.
2. **Work nomination** (#26, **not yet shipped**) — code + telemetry →
   evidence-backed issues. Until this lands, curated issues need the
   trust-label eligibility marker (`SEC-047`) applied manually to reach
   "ready" for the implementation workflow to claim them.
3. **Implementation** (#27, shipped) — claims a ready issue, opens a worktree,
   runs the agentic implement stage (#19, shipped), passes local
   deterministic gates, opens a PR, polls CI to a repass loop (#18, shipped),
   and stops at a reviewer gate.

```sh
../bin/goobers status       # [pending #23] — instance + active runs at a glance
../bin/goobers trace <run-id>  # [pending #23] — one run's journal, human-readable
```

Until `status`/`trace` ship, the run journal is directly inspectable per
`docs/ARCHITECTURE.md` §4 — it's designed to be (`cat`/`jq`/`grep` are
legitimate debug tools at tier 1):

```sh
cat runs/<run-id>/run.yaml
cat runs/<run-id>/events.jsonl | jq .
ls runs/<run-id>/artifacts/
```

### 4. Verify

A human merges the resulting PR (issue #30 requires a human merge step —
Goobers doesn't self-merge at V0). Then confirm telemetry answers "what
happened and what's failing":

```sh
# [pending #24's query surface + #23's CLI wiring — the query package
# (internal/telemetry/query, shipped) is usable programmatically today; a CLI
# front-end is #23's scope]
../bin/goobers telemetry stats
../bin/goobers telemetry errors
```

Confirm: the merged PR traces back to its seed issue (issue↔PR breadcrumb,
#27); the run's journal is complete and internally consistent (`state.json`
matches the terminal event); a deliberately-introduced CI failure on a second
seed issue drives at least one repass before merging.

## V0 milestone checklist

Every bullet from `docs/ARCHITECTURE.md` §12's V0 description, mapped to the
issue(s) that ship it and the runbook step that demonstrates it. Status
reflects `main` as of this writing, not the eventual acceptance run.

| V0 bullet | Issue(s) | Status | Demonstrated in |
|---|---|---|---|
| Install/init locally | #11 | ✅ shipped | Setup |
| Managed working copy, per-run isolation | #16 | ✅ shipped | Run (implicit) |
| Definitions-as-code DSL + config loading | #9, #11 | ✅ shipped | Setup |
| Read/modify GitHub issues | #12 | ✅ shipped | Observe (curation) |
| Open/poll/close PRs | #13 | ✅ shipped | Observe (implementation) |
| Deterministic stages (shell) + ci-poll | #18 | ✅ shipped | Observe (implementation) |
| Agentic stages (Copilot CLI adapter) | #19 | ✅ shipped | Observe (implementation) |
| Stage contract: envelopes + artifact pointers | #10 | ✅ shipped | Verify (journal) |
| Run journal: durability, redaction | #8 | ✅ shipped | Verify (journal) |
| Cron triggers + max-parallel/budget conditions | #21 | ✅ shipped | Run |
| Local telemetry: journal spans + SQLite rollup | #22 | ✅ shipped | Verify (telemetry) |
| Telemetry query surface | #24 | ✅ shipped | Verify (telemetry) |
| Gate execution: automated + agentic, bounded repass | #20 | ✅ shipped | Observe (implementation) |
| Local credential handling, capability scoping | #14 | ✅ shipped | Setup (implicit) |
| Runner core: lifecycle, durability, resume, retries | #17 | 🔶 in review (PR #64) | Run |
| CLI surface: `up`/`run`/`status`/`trace` | #23 | ⬜ not started | Run, Observe, Verify |
| Workflow: backlog curation | #25 | ✅ shipped | Observe (1) |
| Workflow: work nomination | #26 | ⬜ not started | Observe (2) |
| Workflow: implementation, reviewer + CI-poll repass | #27 | ✅ shipped | Observe (3) |
| Self-hosting dogfood config | #28 | ⬜ in progress | Setup |
| E2E walking skeleton (conformance seed) | #29 | ⬜ in progress | (validates the whole chain on fixtures, ahead of this live run) |

**5 of 20 bullets are not yet demonstrable** (#17 in review; #23/#26/#28/#29
not started or in progress) — this runbook cannot be executed for real until
they land. Re-run the checklist after each merge; strike "not yet
demonstrable" once its issue closes.

## Known limitations (V0 → later)

What V0 deliberately does not do, so a reader doesn't mistake a scoping
decision for a bug:

- **No self-merge.** A human merges the PR the implementation workflow opens
  (`ARCHITECTURE.md` §12 roadmap). Full autonomy is out of scope at every
  tier documented so far.
- **No sandboxed stage execution / per-goober credential injection.**
  Isolation is worktree + process only at tier 1 (`ARCHITECTURE.md` §9);
  sandboxing is V1 (tracked as #35).
- **No portal.** The journal and telemetry store are inspectable via CLI/files
  only; the portal reading them is V1 (`ARCHITECTURE.md` §12).
- **cron-only scheduling.** Backlog-item and external-signal triggers are
  modeled but expressed as cron-triggered `backlog-query` stages at V0, not
  first-class trigger types yet (`docs/requirements/scheduler.md`).
- **GitHub only.** Azure DevOps parity is V1 (`BL-033`); `providers/ado.go`
  exists but isn't part of the V0 acceptance path.
- **Structural-only cron/schedule validation.** No range-checking beyond
  charset/field-count at V0 (flagged non-blocking on #50's gate).
- **Agentic harness produces no file artifacts**, only scalar outputs and
  provider side effects (PRs, comments) — tracked as #73 (V1).
- **Registry-scrubber wiring gap on runner-written journal events** (not
  executor-materialized credentials, which are already scrubbed) — tracked as
  #66/re-scoped per QA-1's #64 review, V1.
- **No stricter capability-string canonicalization** — `github:pr:write` vs
  `github:prs:write`-style spelling drift is caught by tests today, not a
  registry — tracked as #74 (V1).
- **Agentic subprocess env is a bare PATH/HOME/TMPDIR allowlist** (default-deny,
  by design — safer than a denylist filter that could miss a credential var),
  which may starve tools that expect `XDG_*`/`LANG`/`SSL_CERT_FILE`/proxy vars
  in less common environments — tracked as #75 (V1).

## Execution record

*(Empty until a clean, recorded execution happens — see the acceptance
criteria in issue #30. This section will hold: the date, operator, seed
issues filed, journal excerpts for at least one clean pass and one
repass-triggering pass, and links to the resulting merged PR(s).)*
