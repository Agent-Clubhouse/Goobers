# V0 Acceptance Runbook

> Status: **All code dependencies shipped; live execution pending.** This is
> the verification artifact for issue #30, the V0 milestone gate, and — per
> epic #130 — the closing acceptance criterion for the V0.1 last-mile
> integration remediation wave. As of `e739bd0` (2026-07-14): all 20 V0
> bullets below are shipped, `#17`/`#23`/`#26`/`#29` (the last four holdouts
> at the previous revision of this doc) are complete, and epic #130's own
> remediation (real subcommands, live `CIPollExecutor`, live `GitHubProvider`
> construction, worktree branch continuity, `prNumber` handoff, ci-gate
> vocabulary symmetry, the Tutor wave T1–T5) is merged and independently
> re-verified (`make ci` green, `-race`, 0 lint; static code-path audit —
> see the epic-#130 remediation checklist below). **One known gap surfaced by
> that audit:** `ref.touched` journal events for real provider mutations
> (PR/issue/claim) never fire in production — `providers.WithMutationRecorder`
> is wired in tests only; only a single per-run branch-touch event reaches the
> journal. Tracked as a known limitation below, not yet filed as its own
> issue. The milestone closes only once someone other than the runner's
> primary implementer executes this runbook clean, end to end, on a real
> target repo, and the [Execution record](#execution-record) appendix is
> filled in with real journal excerpts and PR links (issue #30's acceptance
> criteria) — **that live execution has not happened yet** as of this
> revision; target-repo and credential provisioning are pending explicit
> human confirmation (see #mission-conformance-acceptance, 2026-07-14).
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
2026-07-13/14. This runbook's mechanics are ready to execute for real —
what remains is the live execution itself (see status banner above) plus
epic #130's own remediation checklist below, which this same live run also
serves as the acceptance evidence for.

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
| `TaskExecutor`/`CIPollExecutor` registered but never wired to a real stage | `runnerwiring.go` constructs `CIPollExecutor` against the real `ci-poll` stage-kind | ✅ live, `#132` |
| No `GitHubProvider` constructed on the live path | `providercmd.go`'s `newGitHubProvider` used by all three subcommands + ci-poll's poller | ✅ live, `#132`/`#139` |
| `ref.touched` / claim ledger had zero production callers | Claim ledger: ✅ real (`backlogquery.go --claim`, `up.go`'s `RecoverExpired`). `ref.touched` for provider mutations: ❌ still gap — see Known limitations | 🔶 partial, `#132` |
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
`selfhost/`. Live execution — the actual cron-fired pass this table's own
acceptance bar requires — is pending target-repo and credential
confirmation; see status banner.

## Known limitations (V0 → later)

What V0 deliberately does not do, so a reader doesn't mistake a scoping
decision for a bug:

- **No self-merge.** A human merges the PR the implementation workflow opens
  (`ARCHITECTURE.md` §12 roadmap). Full autonomy is out of scope at every
  tier documented so far.
- **`ref.touched` journal events don't fire for real provider mutations.**
  `providers.WithMutationRecorder` (`providers/github.go`/`seams.go`) exists
  and is tested, but every production call site (`backlogquery.go`,
  `openpr.go`, `issuecloseout.go`) constructs the provider without it — so
  opening a PR, commenting on/closing an issue, or applying a claim marker
  never gets journaled as `ref.touched`. Only one `ref.touched` event fires
  per run in production, the run's own git branch (`runner/run.go`) —
  functionally harmless (the mutations themselves still happen for real) but
  the per-run "which issue/PR did this actually touch" journal traceability
  epic #130 called out is incomplete. Found during 2026-07-14's static
  verification for the #30/#130 closing gate; not yet filed as its own
  issue — worth one before V1 sandboxing work touches this seam again.
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
