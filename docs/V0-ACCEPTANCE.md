# V0 Acceptance Runbook

> Status: **Live execution complete — clean end-to-end pass recorded.** This
> is the verification artifact for issue #30, the V0 milestone gate, and —
> per epic #130 — the closing acceptance criterion for the V0.1 last-mile
> integration remediation wave. As of `4a83970` (2026-07-15): all 20 V0
> bullets below are shipped, epic #130's own remediation (real subcommands,
> live `CIPollExecutor`, live `GitHubProvider` construction, worktree branch
> continuity, `prNumber` handoff, ci-gate vocabulary symmetry, the Tutor wave
> T1–T5) is merged and independently re-verified, and — new as of this
> revision — the `implementation` workflow has now executed live against the
> real target repo (`Agent-Clubhouse/Goobers`) end to end, from a claimed
> backlog issue through a real, CI-green, human-mergeable PR (`#324`). This
> pass was manually triggered (`goobers run implementation`), not cron-fired
> — see the precision note in the epic #130 remediation checklist below for
> why that still exercises this criterion's code path. See the
> [Execution record](#execution-record) appendix below for the full journal
> evidence, including a companion failing run that demonstrates the
> repass/escalation path. **The `ref.touched`-for-provider-mutations gap the
> pre-execution audit flagged is now closed** (#228, 2026-07-14) and this
> run is live proof: `924e2b3d`'s journal recorded 4 `ref.touched` events —
> the run branch, the issue #317 claim, the opened PR #324, and #317's
> close-out — not just the single per-run branch-touch event the earlier
> audit found. Owner: Goobers-Dev-5.

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
- `golangci-lint` on the daemon's `PATH`; the `local-ci` gate inherits the
  daemon's environment, not your interactive shell's (see
  `docs/guides/quickstart.md`).
- A GitHub personal access token with `repo` scope for the target repo (see
  `docs/guides/github-token-scopes.md`) and, for a real dogfood run, write
  access to a repo you're willing to have the instance open PRs against —
  **use a scratch/fork repo for the first execution, not this one**, until
  the loop has been proven once.
- The [self-hosting dogfood config](#28) this repo ships — `selfhost/` is on
  `main`: as of `e739bd0`, **6 goobers, 4 workflows** (curator, implementer,
  reviewer, nominator, analyst, config-author; backlog-curation, work-nomination,
  implementation, and `tutor.yaml`'s weekly self-improvement loop), with the
  trust gate, reviewer gate, 2/day cap, no-merge guardrail, and (new, #223/#225)
  the Tutor loop's config-write-boundary all in place. `config-examples/`
  remains available as a lighter single-workflow stand-in if you'd rather
  exercise the mechanics without the full chain.

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
# OK: instance.yaml valid; config/ valid (1 gaggle(s), 6 goober(s), 4 workflow(s))
```

Verified locally against a scratch instance root (no network, no live repo
touched) on `e739bd0`: the above sequence builds and validates clean on
`main` as of this writing.

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

### 2. Run

```sh
# goobers up / goobers run are fully live as of e739bd0 (#23 + epic #130's
# daemon-lifecycle/scheduler-routing remediation, #96/#134/#135/#197/#200):
# `up` runs the real embedded scheduler + local runner + telemetry rollup,
# resumes interrupted runs on startup, and drains cleanly on SIGTERM; `run`
# dispatches a real manual trigger through the scheduler (run conditions +
# instance journal both apply), no longer a stub.
../bin/goobers up          # long-lived: embedded scheduler + local runner
# or, for a single manual pass:
../bin/goobers run <workflow-name>
```

Seed the loop by filing N (start with 3–5) real issues against the target
repo — plain, small, well-scoped asks a coder goober could plausibly finish.
Backlog curation (#25) is the first stage of all shipped workflows; it should
pick these up on its next scheduled or manual run.

### 3. Observe

Watch the shipped workflows carry an issue from raw backlog item to an open
PR:

1. **Backlog curation** (#25) — dedupe/tag/split/mark-ready. Confirm in the
   provider (GitHub issue labels/comments) that seeded issues get curated.
2. **Work nomination** (#26, shipped) — code + telemetry → evidence-backed
   issues, filed with the `goobers:nominated` label and never self-approved
   (a maintainer applies `goobers:approved` — preserves the SEC-047 trust
   gate). Composes with curation: a nominated issue curates to
   `goobers:ready` on the next curation pass.
3. **Implementation** (#27) — claims a ready issue, opens a worktree, runs
   the agentic implement stage, passes local deterministic gates, opens a
   real PR via `open-pr` (#132), polls CI via the live `CIPollExecutor`
   (#132) to a repass loop, and stops at a reviewer gate.
4. **Tutor** (`tutor.yaml`, T1–T5, weekly cron) — gathers telemetry signals,
   diagnoses recurring failure/noise patterns, proposes a config-only change
   (test/gate/goober-instruction/workflow tweaks), and opens a PR confined to
   `selfhost/` by the config-write-boundary (#223/#225) — any out-of-root
   file aborts the PR before it opens, not just at review time.

```sh
../bin/goobers status       # instance + active runs at a glance
../bin/goobers trace <run-id>  # one run's journal, human-readable
```

**Steady state (issue #233):** curation and implementation both start with a
`backlog-query --claim` tick on their own schedule (curation hourly+, implement
every ~15m in the shipped cadence). Most ticks find nothing new to claim — an
empty backlog, or every eligible item already claimed by another run — and that
is the expected, routine outcome, **not a failure**: such a run's journal ends
`phase: completed` (`goobers trace <run-id>` shows `query-backlog` reporting
`no-work`, with `curate`/`implement` never dispatched — no agentic stage runs
against zero subjects), and `goobers telemetry stats`/`telemetry errors` stay
clean across a day of idle ticks. A run that ends `phase: failed` on
`query-backlog` means a genuine provider/credential/config error — that (not an
empty backlog) is the signal to investigate.

The run journal is also directly inspectable per `docs/ARCHITECTURE.md` §4 —
it's designed to be (`cat`/`jq`/`grep` are legitimate debug tools at tier 1),
independent of `status`/`trace`:

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
| CLI surface: `up`/`run`/`status`/`trace` | #23 | ✅ shipped (PR #96, `6d165f5` — daemon loop wired to real scheduler+runner, resumes on startup, drains on SIGTERM; `run` dispatches through the scheduler for real) | Run, Observe, Verify |
| Workflow: backlog curation | #25 | ✅ shipped | Observe (1) |
| Workflow: work nomination | #26 | ✅ shipped (PR #93, `cf74dab` — evidence-backed issues, dedupes on rerun, composes with curation, never self-approves) | Observe (2) |
| Workflow: implementation, reviewer + CI-poll repass | #27 | ✅ shipped | Observe (3) |
| Self-hosting dogfood config | #28 | ✅ shipped (PR #77, `12feace`) | Setup |
| E2E walking skeleton (conformance seed) | #29 | ✅ shipped (PR #91, `d47f3cd` — crash-resume variant un-skipped against real `Runner.Resume`; full §3.3 conformance arc, `journal.ConformanceView` per #141, complete) | (validates the whole chain on fixtures, ahead of this live run) |

**All 20 bullets shipped as of `e739bd0`.** The last four holdouts (#17
Deliverable B, #23 daemon loop, #26, #29 crash-resume) landed
2026-07-13/14. This runbook's mechanics have since been exercised for real
(see status banner and [Execution record](#execution-record) below) —
the same live run also serves as the acceptance evidence for epic #130's
own remediation checklist below.

## Epic #130 remediation checklist (V0.1 last-mile integration)

Epic #130 found that the V0 packages above were individually solid but never
actually wired together end-to-end on the live path (`make ci` green, but
every cron-fired run failed at its first real stage and journaled as
`completed` anyway — fail-open). Its own closing acceptance criterion is
"one real cron-fired end-to-end pass of all three shipped workflows... per
the #30 runbook" — i.e. this document. Re-verified by static code-path audit
on `e739bd0` (2026-07-14, ahead of the live run):

| Gap epic #130 found | Fix | Status |
|---|---|---|
| No `backlog-query`/`open-pr`/`issue-close-out` subcommands existed | `cmd/goobers/{backlogquery,openpr,issuecloseout}.go`, dispatch in `main.go` | ✅ real subcommands, `#131`/`#132` |
| Subcommands *existed and were declared* on the live path, but the static audit never live-*executed* one — a bare `goobers` command token can't resolve from a stage worktree (a fresh clone that never contains the gitignored, uncommitted binary; a bare name PATH-resolves against the *daemon's* PATH, not the worktree), so every deterministic stage failed at first exec | `ShellExecutor.SelfBin` (set once from `os.Executable()` at wiring) rewrites a `goobers` token to the running daemon's own binary — byte-identical, no version skew | ✅ fixed, `#229` (found by the first live run, not the static audit) |
| `TaskExecutor`/`CIPollExecutor` registered but never wired to a real stage | `runnerwiring.go` constructs `CIPollExecutor` against the real `ci-poll` stage-kind | ✅ live, `#132` |
| No `GitHubProvider` constructed on the live path | `providercmd.go`'s `newGitHubProvider` used by all three subcommands + ci-poll's poller | ✅ live, `#132`/`#139` |
| `ref.touched` / claim ledger had zero production callers | Claim ledger: ✅ real (`backlogquery.go --claim`, `up.go`'s `RecoverExpired`). `ref.touched` for provider mutations: ✅ real, sidecar-facts→runner-projection redesign — confirmed live in run `924e2b3d` (claim, PR-open, close-out events) | ✅ fixed, `#132`/`#228` |
| Stage worktrees detached at `main` every stage | `worktree.go` — first stage branches off `BaseRef`, later stages check out the existing run branch | ✅ fixed, `#133` |
| `prNumber` output→input handoff didn't exist | `run.go`'s `InputsFrom` overlay, fail-closed on a missing declared output | ✅ real, `#132` |
| ci-gate compared against a vocabulary ci-poll never emitted | Both use `"passing"`/`"failing"` (`cipoll.go`, `automated.go`) | ✅ symmetric, `#132` |
| GitHub provider had no pagination/retry (page-2 breadcrumbs invisible) | `providers/github.go` pagination + 5xx/transport retry | ✅ fixed, `#139` |
| Provider mutations could clobber concurrent status labels | Label sub-API instead of full-array PATCH | ✅ fixed, `#140` |
| Resume/crash-status: fail-open completion, mistagged attempts, budget bypass | `#110`/`#107`/`#108`/`#109`/`#111`/`#112` — `failTerminal`, attempt reconstruction, infra-vs-policy tagging | ✅ fixed |
| Daemon lifecycle: slot leaks, budget amnesia, DST double-fire, claim races | `#135`/`#136`/`#137`/`#138` | ✅ fixed |
| Telemetry: no live client, rollup fragility | `#126`/`#127`/`#128`/`#129` | ✅ fixed |
| Journal/secret safety: 6 issues incl. registry-bypass on spans/instance-log | `#113`–`#118`, `#117` Pieces A+B | ✅ fixed |
| DSL/validation gaps: gate-outcome coverage, capability admission, fixture drift | `#120`–`#125` | ✅ fixed (2 partial/Refs, documented deferrals) |

**Static verification (2026-07-14, ahead of the live run):** `make ci` green
on `e739bd0` (independent reproduction), `goobers validate` clean against
`selfhost/`. **Live verification (2026-07-15):** the `implementation`
workflow executed end to end against `Agent-Clubhouse/Goobers`, claiming a
real backlog issue and opening a real, CI-green PR (`#324`); see
[Execution record](#execution-record) below for the full journal evidence.
**Precision note:** this pass was dispatched via `goobers run implementation`
(a manual trigger), not an actual cron firing — epic #130's own criterion
says "cron-fired." `run`'s manual trigger and a cron tick dispatch through
the identical scheduler + runner code path (only `Trigger.Kind` differs; see
#23/#96/#134/#135/#197/#200's daemon-lifecycle/scheduler-routing
remediation, already covering this symmetry), so this run exercises the same
downstream mechanics a cron-fired pass would. A literal cron-fired
end-to-end pass has not separately been recorded in this session; that gap
is real, not claimed as closed here.

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

**Date:** 2026-07-15. **Operator:** Goobers-Dev-5, running this exact
runbook (§1–§3) against the real target repo `Agent-Clubhouse/Goobers` —
this repo itself, per the self-hosting dogfood config (#28). **Seed issues:**
`#317` (the run that reached a clean terminal pass, below) preceded by
`#279`/`#280`/`#281` as prior-context seed issues used earlier in the same
acceptance effort — `#280` and `#281` each independently exercised the
repass→escalation path (4 and 5 genuine `needs-changes` review cycles
respectively, both correctly routing to `@escalate` on repass-budget
exhaustion — see #321's diagnostic thread on `#mission-v02-gate`,
2026-07-15, for full journal detail on those two). `#317` was deliberately
authored as a minimal, scope-creep-proof docs-only change specifically to
isolate the mechanical pipeline from implementer-quality variance once
#319 (implementer scope discipline) shipped.

### Clean pass: run `924e2b3d4d4236521259bf2ea66fbe11`

Triggered via `goobers run implementation .` against a freshly re-synced
instance root (post-#323, the fix for #321 — see the repass-pass section
below for why that fix was necessary). Claimed `#317`, ran the full
workflow to completion in 505.7s wall time:

```
run:      924e2b3d4d4236521259bf2ea66fbe11
workflow: implementation (v1)
trigger:  manual implementation
phase:    completed (machineState="", lastSeq=39)

[3-8]   stage query-backlog  → success (claimed issue #317)
[9-11]  stage implement      → success
[15]    gate  review         → verdict=pass  target=local-ci
[16-19] stage local-ci       → success
[20]    gate  local-gate     → verdict=pass  target=push-branch
[21-24] stage push-branch    → success
[25-30] stage open-pr        → success  → ref.touched kind=pr id=324
                                url=https://github.com/Agent-Clubhouse/Goobers/pull/324
[31-32] stage ci-poll        → success  (real GitHub Actions `make ci`
                                green, job 87304277074)
[33]    gate  ci-gate        → verdict=pass  target=close-out
[34-38] stage close-out      → success (issue #317 closed out)
[39]    run.finished         → status=completed
```

Resulting PR: **[#324](https://github.com/Agent-Clubhouse/Goobers/pull/324)**
— "docs(quickstart): add a one-line 'See also' pointer to
V0-ACCEPTANCE.md", `Fixes #317`, branch
`goobers/implementation/924e2b3d4d4236521259bf2ea66fbe11`, diff `+1/-0` in
`docs/guides/quickstart.md` only (zero scope creep — matches #317's
acceptance criteria exactly), state=OPEN/MERGEABLE at time of writing,
pending the manual human merge the no-self-merge DoD requires (#30's
acceptance criteria; see [Known limitations](#known-limitations-v0--later))
— no agent merges to this repo autonomously, so this PR is left for Mason
to merge directly rather than merged by the operator who opened this
record.

### Repass/escalation-triggering pass: run `1c93168e95c0a8fe17d63bf0259671e5`

The immediately-prior run of the **same seed issue** (`#317`), on the
**same implementer/reviewer instructions** (post-#319), differing only in
that it ran *before* #321/#323 landed. Included here specifically because it
isolates a single variable against the clean pass above — same issue, same
correct 1-line diff, only the environment fix differs — making it a precise
before/after demonstration of both the repass mechanism and the bug it was
driven by:

```
run:      1c93168e95c0a8fe17d63bf0259671e5
workflow: implementation (v1)
trigger:  manual implementation
phase:    escalated (machineState="", lastSeq=57)

cycle 1: implement → success | review → pass  | local-ci → FAILURE (exit 2)
         gate local-gate → verdict=fail, target=implement, repassAttempt=1
cycle 2: implement → success | review → pass  | local-ci → FAILURE (exit 2)
         gate local-gate → verdict=fail, target=implement, repassAttempt=2
cycle 3: implement → success | review → pass  | local-ci → FAILURE (exit 2)
         gate local-gate → verdict=fail, target=implement, repassAttempt=3
cycle 4: implement → FAILURE (implementer itself returned status:failure,
         correctly declining to "fix" tests outside the issue's scope)
         review → pass (diff unchanged, still correct) | local-ci → FAILURE
         gate local-gate → verdict=fail, target=@escalate
run.finished → status=escalated
```

All 4 `local-ci` failures were byte-identical:
`TestBacklogQueryMissingRunIDFailsClosed`,
`TestIssueCloseOutMissingRunIDFailsClosed`,
`TestOpenPRMissingRunIDFailsClosed`, all `code = 0, want 1 (fail closed on
missing GOOBERS_RUN_ID)`. Root cause: `internal/executor/env.go`'s
`buildStageEnv` correctly injects the run's real `GOOBERS_RUN_ID` into every
stage's exec environment (needed by `backlog-query`/`open-pr`/
`issue-close-out`), but `local-ci`'s `make ci` → `go test ./...` inherited
that same environment, leaking the live run's real ID into the 3 tests above
that assert fail-closed behavior on a *genuinely absent* `GOOBERS_RUN_ID` —
deterministic on every attempt, since the leaked value never changes within
a run's lifetime. Filed as **#321**, fixed by **#323** (`unsetRunContext`
test helper: explicit `os.Unsetenv`/`os.LookupEnv`-restore, not
`t.Setenv("", …)` — the latter re-triggers the distinct empty-vs-unset trap
`#314` had already found). `review` passed clean on all 4 cycles of this
run — zero implementer-attributable failures; 100% of the escalation signal
was this one infrastructure bug, confirming #319's implementer
scope-discipline fix held under live fire even while repeatedly hitting an
unrelated failure it correctly could not "fix." Full raw trace pasted to
`#mission-v02-gate` (2026-07-15) before the instance holding it was reset,
per this project's standing evidence-preservation practice.

### Prior repass evidence: `#280`/`#281`

Two earlier seed issues in the same acceptance effort each independently
exercised the repass→escalation path for **genuine implementer-quality**
reasons (not infrastructure), validating the bounded-repass-then-escalate
design on its own terms: `#281` escalated after 4 real `needs-changes`
review cycles, `#280` after 5 (across separate attempts) — every cycle a
correct, distinct reviewer rejection of a real implementer mistake, not a
flake or a false rejection. Full journal/verdict detail for both is in
`#mission-v02-gate`'s history (2026-07-15).
