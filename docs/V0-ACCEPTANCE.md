# V0 Acceptance Runbook

> Status: **Scaffolded, not yet executed.** This is the verification artifact for
> issue #30, the V0 milestone gate. It is structured against the expected
> definition of done now, ahead of its remaining dependencies landing. The
> self-hosting dogfood config (#28), capability registry (#74), and the local
> runner core (#17 — durability/resume/retries merged as `2d75e2e`, issue
> closed) are all on `main`. What's left: **#23**'s daemon loop (`goobers up`
> wired to `Runner.Resume` + graceful drain) and **#29**'s crash-resume variant
> (un-skipping `TestWalkingSkeletonCrashResume` against real `Resume`) — both
> just given the GO now that #17 unblocks them — plus **#26** (work
> nomination, not started). Commands marked **[pending]** below don't behave
> end-to-end on `main` yet even though the CLI surface exists. The milestone
> closes only once someone other than the runner's primary implementer
> executes this runbook clean, end to end, and the
> [Execution record](#execution-record) appendix is filled in with real
> journal excerpts and PR links (issue #30's acceptance criteria) — not
> before. Owner: Goobers-Dev-5.

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
- The [self-hosting dogfood config](#28) this repo ships — `selfhost/` is on
  `main` (PR #77): 4 goobers, 3 workflows (curation, nomination,
  implementation), with the trust gate, reviewer gate, 2/day cap, and no-merge
  guardrails all in place. `config-examples/` remains available as a lighter
  single-workflow stand-in if you'd rather exercise the mechanics without the
  full chain.

## Procedure

### 1. Setup

```sh
# Build the binary (or `go run ./cmd/goobers ...` throughout instead).
go build -o bin/goobers ./cmd/goobers

# Scaffold a fresh instance root.
./bin/goobers init ./my-instance

# Replace the seeded starter config with the dogfood config (#28) — all
# three copies are required (gaggles/ alone fails validate closed with "no
# Manifest object found"); config-examples/ remains a lighter stand-in if
# you'd rather exercise the mechanics without the full chain:
rm -rf my-instance/config && mkdir -p my-instance/config
cp -r selfhost/gaggles my-instance/config/
cp selfhost/manifest.yaml my-instance/config/
cp selfhost/instance.yaml.example my-instance/instance.yaml

# Set the GitHub PAT (never inline it into instance.yaml — the loader
# rejects that, CFG-009/SEC-010) and any other token refs instance.yaml
# names, matching instance.yaml's inline comments:
export GOOBERS_GITHUB_TOKEN=ghp_...

# Validate before anything runs (fails closed on bad config/definitions).
cd my-instance
../bin/goobers validate .
# OK: instance.yaml valid; config/ valid (1 gaggle(s), 4 goober(s), 3 workflow(s))
```

Verified locally against a scratch instance root (no network, no live repo
touched): the above sequence builds and validates clean on `main` as of this
writing.

Before running anything against the target repo, bootstrap its label
taxonomy once (idempotent, `selfhost/README.md` §Setup) — the trust gate
(`SEC-047`) depends on `goobers:approved` existing:

```sh
for l in \
  "goobers:approved:0E8A16:Maintainer-approved — eligible for curation/implementation (SEC-047)" \
  "goobers:ready:1D76DB:Curated and scoped — eligible for implementation" \
  "goobers:claimed:FBCA04:Currently claimed by an in-flight run" \
  "goobers:nominated:5319E7:Filed by the nominator — awaiting maintainer approval" \
  "goobers:needs-human:D93F0B:Needs a decision only a human can make" \
; do
  IFS=: read -r ns name color desc <<<"$l"
  gh label create "$ns:$name" --color "$color" --description "$desc" --force
done
```

### 2. Run **[pending #23 daemon loop]**

```sh
# goobers up / goobers run exist on main (#23, PR #67) but are still honest
# stubs as of this writing: `up` validates + takes the single-instance lock,
# then reports the daemon isn't wired in yet; `run` reports an "escalated:
# local runner not yet wired — no stages executed" result rather than
# silently doing nothing. The local runner itself (#17) is now complete on
# main (`2d75e2e`) — what's left is wiring `up`'s daemon loop to it
# (scheduler + Runner.Resume + graceful SIGTERM drain), in progress. Once it
# lands:
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
../bin/goobers status       # shipped (#23, PR #67) — instance + active runs at a glance
../bin/goobers trace <run-id>  # shipped (#23, PR #67) — one run's journal, human-readable
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
# shipped (#24's query surface + #23's CLI wiring, PR #67)
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
| Runner core: lifecycle, durability, resume, retries | #17 | ✅ shipped (PR #87, `2d75e2e`) | Run |
| CLI surface: `up`/`run`/`status`/`trace` | #23 | 🔶 partial — init/validate/status/trace/telemetry + quickstart shipped (PR #67); `up`'s daemon loop and `run`'s real execution are honest stubs, wiring to #17's now-complete runner in progress | Run, Observe, Verify |
| Workflow: backlog curation | #25 | ✅ shipped | Observe (1) |
| Workflow: work nomination | #26 | ⬜ not started | Observe (2) |
| Workflow: implementation, reviewer + CI-poll repass | #27 | ✅ shipped | Observe (3) |
| Self-hosting dogfood config | #28 | ✅ shipped (PR #77, `12feace`) | Setup |
| E2E walking skeleton (conformance seed) | #29 | 🔶 partial — walking skeleton on real runner Deliverable A shipped (PR #83, `6cb6f05`); crash-resume variant explicitly `t.Skip`'d, un-skip against #17's now-complete `Runner.Resume` in progress | (validates the whole chain on fixtures, ahead of this live run) |

**3 of 20 bullets are not fully demonstrable yet** (#23's daemon loop/real
`run` and #29's crash-resume variant, both now unblocked and in progress
following #17's completion; #26 not started) — this runbook cannot be
executed for real until they land. Re-run the checklist after each merge;
strike "not yet demonstrable" once its issue closes.

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
- **Agentic subprocess env is a bare PATH/HOME/TMPDIR allowlist** (default-deny,
  by design — safer than a denylist filter that could miss a credential var),
  which may starve tools that expect `XDG_*`/`LANG`/`SSL_CERT_FILE`/proxy vars
  in less common environments — tracked as #75 (V1).
- **`instance.yaml` is loaded once, at `goobers up` startup.** Editing it
  (repos, telemetry, `runConditions`) while the daemon is running has no
  effect until the next restart — there is no file watch or SIGHUP-style
  reload (CFG-020/DEP-025 call for this at tiers 1–2; V0.1 scope is
  documenting the restart-required semantics here rather than building
  watch/reload, which is V1, #142). A config that fails `Validate()` is
  caught at that startup load (`goobers up` refuses to start, per `up.go`'s
  `os.Stat(l.ConfigFile())`/`LoadConfig` checks), not silently ignored — the
  gap is strictly "doesn't pick up a later edit," not "runs on bad config."
- **Workflow definitions are pinned at `Version: 1` permanently.** There is no
  mechanism yet to bump a workflow's version when its definition changes;
  `trace`'s `(v1)` display and journal `Trigger.Kind`/gate-outcome comparisons
  (WF-016) key off the run's recorded digest, not this version field, so
  nothing currently depends on it changing — but a reader should not infer
  "unversioned" or "unchanged since v1" from it. Deriving a real version from
  the definition (or its digest) is left for a later pass (#142).

## Execution record

*(Empty until a clean, recorded execution happens — see the acceptance
criteria in issue #30. This section will hold: the date, operator, seed
issues filed, journal excerpts for at least one clean pass and one
repass-triggering pass, and links to the resulting merged PR(s).)*
