# Engine child qualification harness

`qualification_integration_test.go` and
`qualification_worker_integration_test.go` are compiled with `cloudintegration`.
The hosted `cloud-pg` job supplies PostgreSQL 16, Azurite, and a pinned Temporal
CLI. `temporaltest.StartDevServer` starts a real Temporal server; this suite does
not use Temporal's in-memory workflow test environment.

`TestChildQualificationHappyPath` exercises scenario 1 of #8085/#7758:

1. Publish the instance's configuration archive in Azurite with PG metadata.
2. Accept and start the parent through the real start fence, recording run pins.
3. Run production `engine.Run` in a separate worker process. Its scripted parent
   activity acquires a bound grant over HTTP and submits a generated child via
   the authenticated stage child-workflow route.
4. Admit, freeze, load, park, execute, finalize, and complete through production
   control services, PG custody/journals, and a real Temporal child workflow.
5. Resume the parent. It reads the continuation receipt over the pods' blob
   plane using a scoped pod bearer and verifies its digest, size, child identity,
   terminal state, and output. The final parent result proves this read occurred.

The stage executor is scripted instead of invoking an LLM or launching a
Kubernetes pod. The scripted parent also constructs and returns `ChildWait`
itself. Thus #8226 qualifies the engine/control child protocol only: production
stopped-dispatch-to-`ChildWait` adaptation is **not qualified**, pending the cloud
worker handoff adapter PR (`p4-child-handoff`). That adapter and a scenario using
it are prerequisites for enablement. Real Kubernetes qualification remains
scenario 10. Control uses the runtime controller database role;
the worker subprocess receives only the Temporal address, HTTP endpoint, and
signing-key file, with no PostgreSQL or Azure credentials. All persistent custody
and journal operations use the production HTTP routes and backend adapters.
The scheduler is the production `localscheduler`/`SchedulerCapacity`; the fixture
reserves the parent's slot after starting it rather than running trigger polling.

The production config-load refusal and default remain unchanged. The harness
loads a normal fixture config and enables children only on its in-memory instance
config and the parent's pinned `RunInput`. Retained generation contents still pass
normal validation. No process-wide switch or production bypass is installed. The parent's permission
travels in the versioned admission command and is frozen into the child manifest.
Production `Run` verifies the child's Temporal lineage and loads this digest-bound
permission before executing. Neither the current config nor an unverified child
input boolean can enable a frozen child. The accompanying production-entry unit
tests cover enabled and disabled manifests and a disabled parent.

## Adding scenarios

Add a `qualification_<scenario>_integration_test.go` in package `childengine`:

- `newQualificationHarness(t)` creates an isolated database, blob container,
  Temporal server, control HTTP server, configuration, and scheduler.
- `StartParent()` confirms the start pins, reserves capacity, starts a separate
  worker, and returns the real Temporal `client.WorkflowRun`.
- `KillWorker()` kills and joins only the harness-owned worker PID;
  `StartWorker()` replaces it using the same task queue and durable services.
- `InstallHTTPBarrier(match)` holds the first matching HTTP response **after**
  the real route finishes, before the client receives it. `Wait(ctx)` observes
  that boundary; `Release()` delivers the original status, headers, and body.
  Check the durable state before treating arrival as a successful operation.
  This barrier is outside `routerMu`. Do not block an inner route handler:
  routing holds `routerMu.RLock` through `ServeHTTP`, so an inner gate would
  deadlock `FailoverControl`, which needs its write lock to retire the epoch.
- `StageBarrier(name)` creates a named worker-stage barrier. Before starting
  a worker, call `SetStageScript(qualificationStageScript{...})`; the script is
  serialized into the subprocess configuration. Each phase accepts a `Barrier`
  name, an optional `Result` (`*apiv1.ResultEnvelope`), or an `Error` string.
  After release, an empty outcome continues the normal happy-path script.
  Phases are `parent-start`, `parent-granted` (live grant, before submission),
  `parent-submitted` (admission accepted, before returning the wait handoff),
  `parent-resumed` (after reading/verifying the receipt), and `child` (before
  the child stage returns). An error follows normal activity retry policy.
  Named barriers hold every visit, including worker retries, until released;
  scripts persist across worker replacement. Change scripts only while stopped.
  Scripted stages heartbeat immediately and every second, including while a
  barrier or HTTP response is held and during cancellation cleanup. The worker
  uses production's eight-second maximum heartbeat RPC throttle, so heartbeat
  replies deliver cancellation and the blocked request exits with it. A phase's
  explicit `HeartbeatLoss: true` suppresses heartbeat generation until the next
  phase or the activity ends; use it only for heartbeat-loss injection. A killed
  worker naturally stops heartbeating. Merely holding a barrier does not.
- `FailoverControl()` closes the old HTTP/journal owner, releases the lease, and
  composes a successor epoch. Scheduler capacity is reconstructed from Temporal
  liveness and committed child-wait journal events. This models voluntary
  takeover; a scenario testing lease expiry must inject that fault explicitly.
- `ResetParent(run, workflowTaskCompletedEventID)` issues the Temporal reset RPC.
- `ReloadConfig(edit)` edits the fixture definition directory, reloads both the
  definition set **and instance.yaml**, records the archive, and atomically
  changes the applied candidate. It preserves the test-instance opt-in.
  `ReloadInstanceConfig(edit, enabled)` passes the instance.yaml filename to
  `edit` and sets the explicit test-instance opt-in after production validation;
  use `false` to test rollback. Both reloads retain old inputs/archives and read
  real files; neither runs the directory watcher. Do not write `enabled: true`
  into instance.yaml while the production config-load refusal remains.
- `SetSlotLimit(n)` reloads scheduler capacity without changing run inputs.

The fields expose the real backend/client for assertions and narrowly scoped
fault injection. Scenario tests must synchronize on an observed durable boundary,
not a guessed delay. Keep the control/worker implementations shared; do not copy
or replace custody with mocks. Cleanup joins the worker and lease renewal,
closes HTTP/journal resources, stops Temporal, and deletes the test blob container
and database. A missing dependency fails with `TESTDEP_STRICT=1` in hosted CI.

For example, to kill the worker with a live stage grant before child submission:

```go
h := newQualificationHarness(t)
barrier := h.StageBarrier("live-grant")
h.SetStageScript(qualificationStageScript{
    "parent-granted": {Barrier: "live-grant"},
})
run := h.StartParent()
if err := barrier.Wait(h.ctx); err != nil {
    t.Fatal(err)
}
// Assert durable binding, kill the worker, then test the old grant's refusal.
h.KillWorker()
barrier.Release()
h.StartWorker()
// Assert the scenario's expected workflow and custody outcome using run.
_ = run
```

HTTP and stage barriers unblock on request cancellation and are released during
test cleanup. An HTTP barrier models a delayed response, not cancellation of
the already-committed operation. These hooks are fixture HTTP endpoints only;
they do not add routes or fault switches to production.
Control and HTTP-server errors are forwarded to the test log. The PG live writer
uses the same configured sentinel root as production's `cloudJournalRunsRoot`;
it does not write a file journal. This avoids rejecting the first journal emit
before the PG backend is consulted.

Before declaring a scenario qualified, read its passing event (including any
subtests) from the hosted `cloud-pg-test-events` artifact. A compile-only check or
skipped local test is not qualification evidence. Scenarios 2–9 and the kind
nightly remain unqualified by this happy-path test.
