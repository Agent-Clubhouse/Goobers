# `internal/localscheduler` — the embedded scheduler (tiers 1–2)

Issue #21. `docs/ARCHITECTURE.md` §7, `docs/requirements/scheduler.md` (SCH-*).

Embedded in the local runner daemon (`goobers up`) — no separate service, no
database. It owns three things:

1. **Cron evaluation** — fires a workflow's schedule trigger when due.
2. **Run conditions** — max-parallel and per-workflow run budgets, enforced
   before a run starts.
3. **The claim ledger** — the atomic, lease-based source of truth for
   exactly-once backlog-item processing.

Distinct from `internal/scheduler`, the earlier Temporal-era (M7) scheduler —
quarantined as a tier-3 component (§11, #15) and revived, not reused, at V2.

## Cron evaluation

`ParseSchedule` wraps `robfig/cron`'s `ParseStandard`: standard 5-field cron,
the named descriptors (`@hourly`, `@daily`, ...), and `@every <duration>` — the
same grammar `internal/workflow.CheckSchedules` structurally validates at
compile time (which also accepts 6-field cron with a seconds column, since it's
a looser structural gate). A 6-field expression is rejected here with an
actionable error: V0 owns firing on 5-field cron only, so it fails closed
rather than silently misinterpreting a seconds column.

`Schedule.Next(after)` is DST-correct because it does calendar arithmetic in
`after`'s location, not fixed-duration wall-clock math (`InLocation` pins the
instance-configured timezone regardless of what location a caller's clock
happens to hand in). Verified empirically against `America/New_York` 2026:
a schedule whose time doesn't exist on the spring-forward day skips to the next
valid day; a schedule at a safe hour stays wall-clock-stable across the
transition; a schedule inside the repeated fall-back hour fires once at each
UTC offset it maps to (documented, not a bug).

**Missed-tick policy**: `Tick(TriggerState, now)` collapses any number of
fires that fell inside `[LastEval, now)` into exactly one catch-up — `LastEval`
advances to `now`, never to the next unfired tick, so no backlog of stale fires
replays on the following tick. `ReconstructLastEval` derives each workflow's
`LastEval` baseline after a restart from the instance journal's
`trigger.fired` history (or daemon-start time for a trigger never observed —
no epoch backfill).

## Run conditions

`Conditions.Admit` is an atomic check-and-reserve: max-concurrent-runs
(default 1, per `apiv1.ReadinessConditions`) and a rolling-hour run budget,
checked and reserved in one critical section — the property that makes
max-parallel hold under simultaneous ticks, not just sequential calls.
Exhaustion skips (never fails), returning a stable reason
(`ReasonMaxParallel` / `ReasonBudget`) the caller journals.

`Conditions`' in-memory counters don't survive a restart; `ActiveRunCounts`
scans `runs/` via the journal reader (non-terminal `state.json` phase = active)
to reconcile them at daemon startup.

## The claim ledger

`ClaimLedger` is the authoritative, atomic, lease-based source of truth for
exactly-once backlog-item processing (SCH-020/BL-005). The provider-visible
marker (`providers.ClaimWorkItem`, #12) mirrors it for human visibility once a
local claim succeeds — the ledger has no dependency on the `providers` package
and is never the one asked "who owns this item," only ever the one that
decides.

- **Durable**: a single JSON file under the instance root, rewritten
  atomically (`journal.WriteFileAtomic`, exported from #8's `fsio.go` rather
  than duplicating that durability primitive) on every mutation.
- **Atomic**: an in-process mutex, correct for the single-embedded-scheduler
  architecture (SCH-040 — no separate scheduler service means no cross-process
  claim races to guard against).
- **Lease + expiry**: `Claim(itemID, runID, workflow, leaseDuration)` refuses a
  different run while a lease is live; the *same* run re-claiming (a retried
  backlog-query stage attempt) renews it. `RecoverExpired(now)` releases every
  lease past its expiry — call once at startup (recovers leases orphaned by a
  crash) and periodically thereafter (catches a live run that overran its
  lease without crashing). A released item is claimable again exactly once.

Claim/release transitions journal to the instance log via
`journal.EventClaimAcquired`/`EventClaimReleased`.

## The instance journal

Every scheduling decision — `trigger.fired` (with `Reason` noting a catch-up),
`tick.skipped` (with the run-conditions reason), `run.started`/`run.finished`,
and the claim ledger's transitions — appends to
`<instance-root>/scheduler/events.jsonl` via `journal.InstanceLog` (an
additive #8 extension: same envelope, same append+fsync durability, same
torn-tail crash recovery as a run's own `events.jsonl`). `cat`/`jq` work here
exactly like they do on a run journal:

```sh
jq -c 'select(.type=="tick.skipped")' <instance-root>/scheduler/events.jsonl
jq -c 'select(.type=="trigger.fired" and (.reason | startswith("catch-up")))' \
  <instance-root>/scheduler/events.jsonl
```

## The Starter seam (→ #17)

`Scheduler` dispatches an admitted, due workflow through `Starter.Start` — the
"start a run" call into the local runner (issue #17). One `Starter` per
workflow (the runner is bound to a single compiled machine at construction), so
`Scheduler` holds a `WorkflowEntry` per workflow, not a single global starter.

`StartRequest`/`StartResult` mirror `runner.StartInput`/`Result`
field-for-field where they overlap (`RunID`, `Gaggle`, `Trigger`, `RepoRef`,
`Item`) — coordinated live on `#mission-runner-core` — so wiring a
`*runner.Runner` as a `Starter` is a straight field copy, not a translation
layer. They're declared locally (not imported from `internal/runner`) so this
package builds and tests independently of #17's landing order; a small
adapter closes the gap once both land.

The scheduler generates `RunID` (`telemetry.NewRunID`) *before* calling
`Start`, since it needs the id as the claim-ledger key before the run exists —
the runner creates the journal using that exact id, so claim-ledger identity
and run identity are the same value throughout, no reconciliation step.
`Start` runs synchronously to completion (or a human-gate pause) on #17's side;
the scheduler calls it in its own goroutine so the daemon loop is never
blocked on a run, releasing the workflow's admitted slot when it returns.

## No busy-polling

`Scheduler.Run` computes the earliest next-due trigger across every
cron-managed workflow and sleeps until then (floored at 1s so clock jitter on
a just-fired schedule can't spin the loop) — a single `select` on `ctx.Done()`
and a timer channel, never a hot loop.
