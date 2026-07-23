# Design: Unattended operation — an instance that survives a week without an operator

> Status: **Draft for review — prescriptive** · Area prefix: `UNOP` · Milestone: _proposed_
> **Unattended Operation**
> Origin: every load-bearing availability failure across the 2026-07 run-watch corpus
> (`~/source/Goobers-Review/`): a silent daemon death with no watchdog
> (observation_20260722_1803), merged fixes undeployed for 20+ hours (#1166) or
> indefinitely (#1278/#1280), restart-orphaned agent processes (#499), an unbounded
> `runs/` tree (18k+ dirs, #149/#550), completion metrics inflated 5–10× by no-work ticks,
> and recurring rate-limit outages whose cost scales with backlog size (#1053).
> Grounding: `docs/guides/supervision.md` (DEP-Q6 resolution), `internal/platform/proc`,
> `internal/configdiff` / `goobers config diff`, `cmd/goobers/up.go` drain path,
> `internal/localscheduler/providerquota.go`.
> Related requirements: `DEP-*` (deployment), `TEL-013` (retention), `SCH-*`.

## 1. Why this exists

The loop works: the instance curates, implements, reviews, and merges autonomously, and
has repaired its own observed bugs end-to-end. What it cannot yet do is **stay alive and
truthful without a hands-on operator**. Every watched round ended the same way: not with
an agentic failure, but with an operational one — the daemon dead and nobody to restart
it, a fix merged to main that never reached the running binary, orphaned child processes
holding credentials, a disk quietly filling, a dashboard number that counted empty ticks
as work.

For the solo builder in the vision ("a gaggle working their hobby projects while they
sleep"), these are not polish items; they are the product. This design makes *unattended
survival* a first-class, testable property.

**Definition of done for the wave:** a Goobers instance runs for 7 consecutive days with
zero operator interventions — supervised restarts only, disk bounded, zero orphaned
processes, and `goobers status` numbers that match a manual journal audit.

## 2. Principles

1. **Supervision is the platform's job, not the operator's memory.** The OS supervisor
   (systemd/launchd/Windows service) owns liveness; Goobers ships the integration, not a
   document alone.
2. **A fix is not delivered until the running instance has it.** The instance closes its
   own deploy loop, policy-gated.
3. **Every lifecycle exit is a guarantee, not a happy-path side effect.** Shutdown,
   startup, and crash paths reap children, release claims, and record honest state.
   (The recurring "cleanup coupled to a happy path" smell from the run-watch corpus.)
4. **Honest numbers beat good numbers.** Metrics changes that make graphs step *down*
   (removing no-work inflation) are corrections, and the design says so up front.
5. **External quota is a budget, not a surprise.** Provider API spend is measured,
   attributed, and bounded per tick — cost must scale with work done, not backlog size.

## 3. Workstreams

### UNOP-1 — Supervised daemon by default

DEP-Q6 is resolved and the unit files ship (`packaging/systemd/`, `packaging/launchd/`,
`internal/winsvc`; `docs/guides/supervision.md`) — but installing them is still a
manual, documented procedure. Productize it:

- `goobers service install|uninstall|status` installing/managing the shipped platform
  unit (systemd / launchd / Windows service), wiring the existing graceful-shutdown
  contract (SIGTERM → drain → exit).
- Crash-loop protection: supervisor-side restart backoff in the generated unit;
  daemon-side "dirty restart" detection (previous up.lock present + no clean shutdown
  event) journaled to `scheduler/events.jsonl` so restarts are observable.
- Liveness that can't lie: `up.lock` gains a heartbeat (mtime or embedded timestamp
  refreshed per tick) and `/api/v1/health` reports last-tick age; both are what
  `goobers status` and the dashboard trust — never pid existence alone. (The
  weekend-era lesson: a stale up.lock with a live-looking pid, twice.)

### UNOP-2 — Self-update loop (the riskiest item; design gate before build)

The watched pattern: the daemon merges its own fix, then runs the old binary for a day.
Close the loop, conservatively:

- A deterministic `self-update` workflow (ordinary workflow, cron-triggered): detect new
  target version (policy: `manual` | `on-release` | `on-main`), build/fetch to a staging
  path, run `goobers validate` + smoke (`--version`, `config diff`) against it, then
  hand off to the supervisor for drain-and-restart onto the new binary.
- **Rollback story is mandatory**: previous binary retained at a well-known path;
  post-restart health gate (N clean ticks) or automatic revert + escalation issue.
- Config drift check on every startup: `goobers config diff` against the shipped
  canonical config, warn-level journal event + dashboard surface on drift (the #825
  drifted-fork class dies here; hard-fail stays opt-in since instance-local divergence
  is legitimate).
- Non-goal: updating the *config* repo — that is Workflow CD (M15); this workstream
  moves only the binary.

### UNOP-3 — Process-tree hygiene

- Drain kills the full child tree via `internal/platform/proc` (sessions already
  detached per the Setsid fix); shutdown refuses to report clean while children remain.
- Startup reaper: identify and terminate orphaned harness processes from prior
  instances (parented to init, holding provider connections) before admitting work —
  closes the #499/#848 class observed at 1–5 orphans per restart.
- Windows parity via the Job Objects follow-up chain (#1090).

### UNOP-4 — Retention & disk hygiene

- `goobers runs prune` (policy: age + terminal-state + keep-last-N per workflow;
  never prune non-terminal or escalation-referenced runs), honoring TEL-013's
  configured retention window; `runs du` already ships (#550).
- `scheduler/events.jsonl` and telemetry rollup rotation under the same policy.
- Prune is journaled (what was removed, by what policy) — provenance survives as a
  tombstone even when payload ages out.

### UNOP-5 — Truthful status & metrics

- Separate the **no-work tick** outcome from genuine completion at the source
  (runner/journal), not in consumers: a `run.finished` disposition axis
  (`completed-work` / `completed-no-work` / …) per the outcome-vs-execution split
  (#851), projected through rollup, `goobers status`, and the dashboard.
- Re-baseline dashboards/stats on the honest axis; document the expected step-down.
- Silent-failure visibility: failure classes currently visible only in status strings
  (e.g. local-ci sync conflicts) become typed error events monitors can query.

### UNOP-6 — Provider API cost architecture

- One shared PR-list/issue-list fetch per tick fanned to consumers (merge-review +
  pr-remediation are 81% of runs and each re-fetch today); conditional requests
  (ETag/If-Modified-Since) on list endpoints.
- Eliminate per-run provider bookkeeping on empty ticks (measured ≈2 writes/run at
  ~7k runs).
- Per-provider budget ledger with reset-aware degradation (extends
  `providerquota.go`): when the remaining quota window can't cover a full tick,
  shed lowest-priority polling first and journal the shed decision. (#1053)
- Webhook triggers (already shipped) preferred over polling wherever the repo allows —
  each conversion is both a cost and a latency win; full event-driven breadth is a
  W5/V2 item, but merge-review's PR-event consumption belongs here.

### UNOP-7 — Daemon identity

- A distinct bot identity (GitHub App preferred; machine-account PAT fallback) for all
  daemon mutations. Removes: attribution-by-head-branch heuristics (`mergedBy` is
  useless today), the self-review 422 class (#870's workaround), and ambiguity in
  incident forensics (#797-class questions become answerable).
- Prerequisite for mixed-mode actor classification (#805) — you cannot classify actors
  while the daemon shares the operator's identity.
- Migration: identity is configuration (`instance.yaml` credential refs); selfhost
  migrates first; single-token remains supported for tier-1 friction-free start.

## 4. Phasing & dependencies

| Phase | Items | Rationale |
|---|---|---|
| 1 | UNOP-1, UNOP-3, UNOP-4 | Independent, small blast radius, immediately testable; the "survive a crash, bounded disk" floor. |
| 2 | UNOP-5, UNOP-6 | Honest telemetry before self-update (the health gate in UNOP-2 needs trustworthy signals); cost work before higher throughput re-tests. |
| 3 | UNOP-2, UNOP-7 | Self-update rides on supervision (1) + honest health (5). Identity is independent but sequenced here to pair with its mixed-mode consumers. |

The 7-day unattended soak (§1) is the wave's acceptance run and should be executed as a
watched round with the run-watch methodology.

## 5. Open questions

- **UNOP-Q1:** Self-update source of truth — git main of the instance's own product repo,
  or tagged releases only? (Selfhost wants main; adopters want releases. Policy knob,
  but which is default?)
- **UNOP-Q2:** GitHub App vs PAT for the daemon identity at tier 1 — is App setup
  friction acceptable for solo builders, or does the tier-1 default stay single-token
  with identity as a tier-2 recommendation?
- **UNOP-Q3:** Prune tombstones — full event-index tombstone vs a single prune event
  with counts? (Journal-size vs forensics trade.)
- **UNOP-Q4:** Should `completed-no-work` runs write a journal at all, or become a
  scheduler-journal-only event (removing ~90% of run-dir churn at the source)?
