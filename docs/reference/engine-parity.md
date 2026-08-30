# Engine parity: what a tier-3 engine run does and does not share with a runner run

Decision 005 D1 (#3876) made the tier-3 Temporal engine reachable from real triggers — cron,
webhook, and the trigger plane — on an engine-enabled instance. Before it, `goobers
engine-start` was the only way to reach the engine walk, so every parity question was
academic. It is not academic any more: on an engine-enabled instance a scheduled workflow
whose stages are fully pinned is dispatched onto the engine by the same scheduler tick that
dispatches every other workflow onto the local runner.

This page records what that means concretely — which facts are pinned into an engine run,
which are not yet, and what the CLI's routing does — so an operator reading a run's
`run.yaml` knows exactly how much of it is authoritative.

## Selection: which runs go to the engine

Selection is **per workflow entry**, decided once at scheduler-definition build time from
the workflow's own pins (`bootstrap.PinStagePlacements`). An entry gets the engine Starter
only when all of the following hold:

- the instance has engine configuration and a reachable engine client;
- the workflow yields a **non-empty** pin set;
- **no** stage is pinned to `self`; and
- **every agentic gate** in the workflow is pinned.

Anything else keeps the local runner, wrapped so that each dispatch annotates
`starter=runner` with the reason it fell back. The fallback is deliberately per-lane:
rollback is a DSL edit to one workflow, not an instance-wide flag, and a self-pinned lane
is **not** globally refused — every 2.0 lane self-pins today, and refusing them as a class
would take four lanes out at once.

The `starter=` annotation on each tick is the observable contract. If a lane you expect on
the engine is running on the runner, that annotation names the predicate that rejected it.

## What an engine run pins

An engine run's identity carries the same provenance a runner run's does. In particular
`GooberDigest` — the content digest of the goober kit — is pinned into the engine's
`StartSpec`, the workflow's `RunInput`, and the resulting run identity, so an engine run's
`run.yaml` names its kit exactly as a runner-driven run does and the two are comparable in
the parity harness.

Claim partitioning is pinned the same way: `BacklogQueryAssignedTo` and
`BacklogQueryRequireLabels` carry the instance's effective self identity and the gaggle's
required labels, which is what gives an engine run the MIRC-2 claim partition the runner has
had since #1901.

## The gap: kit selection is not yet by digest (#3884)

`GooberDigest` is **descriptive, not prescriptive**. It records which kit the run was
admitted against; it does not select the kit the worker executes. A worker resolves its kit
from its own mounted config, so a kit change that lands mid-flight can still be observed by
an in-flight engine run — the run's `run.yaml` will name the digest it started with while
its later stages executed against the new one.

For a runner-driven run this cannot happen: the runner resolves the kit in the same process
that recorded the digest.

This gap is tracked as **#3884** and was deliberately left outside D1's scope. Closing it
means the worker resolving its kit *by the digest the run pinned* rather than from ambient
config. Until then, treat `GooberDigest` on an engine run as "the kit this run was admitted
against", and avoid landing kit changes while long engine runs are in flight if exact
stage-level kit provenance matters to you.

## `engine-start` routing and dedupe

`goobers engine-start` has two modes, and which one you get depends on whether a `goobers
up` daemon holds the instance lock.

**Delegated (default when a daemon is up).** The dispatch is handed to the daemon, which
admits the run through the scheduler. The run therefore takes a concurrency slot, records an
instance-log `run.started`, reserves the run journal before Temporal is called, and fires the
terminal hooks on completion — claims release, circuit breaker, abort labels, the lot. The
run id is the scheduler's. This is the same #343 rule `goobers run` has always followed for
the runner; D1 extends it to the engine now that the daemon can start engine runs itself.

`--dedupe-key` is **refused** on this path rather than silently ignored. The trigger plane's
request id dedupes *deliveries* — it stops one request being dispatched twice — and the
scheduler mints a fresh run id on every admission, so there is no unit-of-work identity for
a dedupe key to name. Accepting the flag and dropping it would let an operator believe two
dispatches had collapsed into one run when they had not.

**Direct (`--direct`, and implied when no daemon is running).** The workflow is started
straight on Temporal with `REJECT_DUPLICATE`, deriving the run id from gaggle, workflow, and
`--dedupe-key`. This is the only mode in which `--dedupe-key` means anything: a direct
start's run id *is* its dedupe unit. A direct start takes no scheduler slot and fires no
terminal hooks — it is a debugging and bootstrap tool, not an operational path.

## Restart and the start-to-first-emit window

The window between "the daemon decided to start an engine run" and "the workflow's first
journal event lands" is closed from both sides:

- **Disk side.** `engine.ReserveRun` writes the run's reservation and header *before*
  Temporal is called, using the same recorder the workflow itself would use, so the record
  is byte-identical either way and whichever writer lands first authors `run.yaml`
  permanently. A daemon that dies in the window leaves a run directory behind, not nothing.
- **Engine side.** On boot the daemon scans open engine workflows for the gaggles it owns
  and keeps the run-id → workflow-id mapping (`engine.OpenRuns`). This matters most for a
  *scheduled* engine run, whose workflow id is not its run id: without the mapping the
  daemon describes the run id, gets `NotFound`, treats `NotFound` as settled, and releases
  the scheduler's concurrency slot underneath a live workflow — which then invites a second,
  duplicate driver for the same workflow. An open workflow with no local run directory is
  reported to the instance log as an orphan; it is deliberately **not** cancelled, because a
  workflow this process cannot account for is not one to terminate on a guess.

The gaggle filter on that scan is fail-closed: a daemon that has not said which gaggles it
owns is told about no runs at all, so sibling instances sharing one Temporal namespace never
reattach to each other's work.

## Operator interventions: the `goobers.hitl.v1` protocol (#3883)

Approve, override, rerun-stage and deny reach an engine-driven run over a versioned Temporal
**workflow Update**, not through the in-process runner. The daemon translates the same HTTP
intervention request into an `engine.HITLIntent`, delivers it with
`WaitForStage: WorkflowUpdateStageCompleted`, and reports success only once the workflow has
durably accepted and acted on it. Runner-driven runs are untouched by this path; a daemon with no
engine client keeps the #3847 `run_engine_driven` refusal.

The protocol, its phase/refusal matrix, its idempotency and terminal-generation rules, and its
rollback story are specified in
[`docs/design/human-in-the-loop.md` §4a](../design/human-in-the-loop.md).

**Named parity drift.** An engine run numbers a stage's *dispatch* attempts per-dispatch starting at
1 (`dispatchWithRetry`); a runner run numbers them cumulatively across the run. The
parity-observable record is identical — an operator rerun writes `stage.rerun.requested` with the
same cumulative `attempt` and `attemptClass: human` the runner writes — but the attempt number
observed *inside* a re-dispatched stage differs. Threading a cumulative attempt base through the
dispatch path is out of #3883's scope and is tracked separately.

**It is opt-in.** `engine.hitl.enabled` defaults to false. While a hold is open the run's Temporal
workflow stays open and its scheduler concurrency slot stays occupied, so an instance turning it on
also chooses the window it can afford (`engine.hitl.window`, 24h by default). The run's *journal*
is unaffected either way: the terminal is written before the hold, not after it.
