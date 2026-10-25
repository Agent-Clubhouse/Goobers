# Engine child qualification harness

`qualification_integration_test.go` and
`qualification_worker_integration_test.go` are compiled with `cloudintegration`.
The hosted `cloud-pg` job supplies PostgreSQL 16, Azurite, and a pinned Temporal
CLI. `temporaltest.StartDevServer` starts a real Temporal server; this suite does
not use Temporal's in-memory workflow test environment.

`TestChildQualificationHappyPath` exercises scenario 1 of #8085/#7758:

1. Publish the instance's configuration archive in Azurite with PG metadata.
2. Accept and start the parent through the real start fence, recording run pins.
3. Run production `engine.Run` in a separate worker process. Its remote parent placement
   journals its stage origin through the signed worker-host seam (control matches
   the active Temporal activity and recorded batch), then runs production
   `Activities.DispatchStage`, which lends the bound grant. The
   scripted container submits a child through the authenticated stage route;
   the production observer reads accepted custody, stops the parent executor,
   and authors `ChildWait`. History assertions require the frozen enable pin,
   two production dispatch invocations, and exactly one trusted handoff.
4. Admit, freeze, load, park, execute, finalize, and complete through production
   control services, PG custody/journals, and a real Temporal child workflow.
5. Resume the parent. It reads the continuation receipt over the pods' blob
   plane using a scoped pod bearer and verifies its digest, size, child identity,
   terminal state, and output. The final parent result proves this read occurred.

The harness runs the real Kubernetes dispatcher against a test-only HTTP
`PodAPI`. The fixture owns simulated pod objects, container executors, and a
shared surrender directory, independently of worker subprocesses. Creation
assigns an immutable UID to the production-rendered pod. Adoption uses the real
`Dispatcher.AdoptPod` identity checks and joins that existing execution; it never
creates a replacement or repeats parent submission. Namespace comes from the
instance's gaggle configuration, and the shared fixture instance ID is a valid
durable ID so production renders its instance label.

Only container execution, kit content, and Kubernetes API responses are
simulated. Grant acquisition, polling control-owned acceptance, exact-UID
adoption, fenced grant revocation and stopped-handoff sealing, and `ChildWait`
construction use production code. On deletion the simulated executor is canceled
and joined before its container reports `Terminated`. The retained finalizer,
isolated production pod spec and same UID are checked by the real dispatcher
before `ParentWritersStopped` can become true. A running or replaced object
cannot supply stop proof. The fixture does not qualify real Kubernetes/kubelet
behavior; scenario 10 remains required for that. No fixture constructs `ChildWait`.
Control uses the runtime controller database role;
the worker subprocess receives the Temporal address, fixture HTTP endpoint, signing-key file,
namespace mapping, and shared surrender directory, with no PostgreSQL or Azure
credentials. All child admission custody and journal operations use the production HTTP routes and backend adapters.
The scheduler is the production `localscheduler`/`SchedulerCapacity`; the fixture
reserves the parent's slot after starting it rather than running trigger polling.
Each control epoch composes its PG `journalpg.SchedulerLog` with a lease-bound
`cloud/claims.Store` wrapped by the shared `localscheduler.SkewTolerantState`
through `WithSchedulerState`, matching cloud control's scheduler-state wiring.
The shared wrapper clamps writes to a predecessor's later evaluation cursor
when a successor's clock is behind. Startup and takeover reconcile those durable snapshots;
slot-limit reloads never fall back to file scheduler state.

The production config-load refusal and default remain unchanged. The harness
loads a normal fixture config and enables children only on its in-memory instance
config and the parent's pinned `RunInput`. Retained generation contents still pass
normal validation. No process-wide switch or production bypass is installed. The parent's permission
travels in the versioned admission command and is frozen into the child manifest.
Production `Run` verifies the child's Temporal lineage and loads this digest-bound
permission before executing. Neither the current config nor an unverified child
input boolean can enable a frozen child. The accompanying production-entry unit
tests cover enabled and disabled manifests and a disabled parent.
The input also carries the workflow's annotations, which participate in its
digest. Ordinary starts and frozen-child manifests preserve the full definition;
the engine's version gate retains the prior journal digest on old histories.

## Adding scenarios

Add a `qualification_<scenario>_integration_test.go` in package `childengine`:

- `newQualificationHarness(t)` creates an isolated database, blob container,
  Temporal server, control HTTP server, configuration, and scheduler.
- `StartParent()` confirms the start pins, reserves capacity, starts a separate
  worker, and returns the real Temporal `client.WorkflowRun`.
- `KillWorker()` kills and joins only the harness-owned worker PID;
  `StartWorker()` replaces it using the same task queue and durable services.
  Simulated pods and their executors survive the kill. Surrenders remain keyed
  by run, stage, and physical `IdentityAttempt`; adoption increments neither
  the logical attempt nor `PodAttempt`.
- `WaitForPodCustody(run)` waits for a custody-bearing heartbeat in Temporal's
  pending activity details and checks its UID against the retained pod. Call it
  after a stage barrier and **before killing the worker** to test adoption.
  Observing the local callback or the initial heartbeat is insufficient: the
  initial heartbeat precedes creation, and RPC delivery is throttled to eight
  seconds. Production workflow recovery passes that recorded custody into the
  replacement `DispatchStage`; the replacement never reconstructs it from disk.
- `SetPodCustodyReporting(false)` explicitly disables receipt reporting for a
  missing-custody fault scenario. Reporting defaults to **on**. Set it before
  starting the worker. The real dispatcher still creates the pod, but its
  engine-installed custody callback is suppressed. A killed worker then produces
  a real heartbeat timeout without custody and production fails closed with
  `childHandoffFailure`, without fresh dispatch. This hook does not revoke or
  expire the grant; assert the actual durable grant state in the scenario.
- `InstallHTTPBarrier(match)` holds the first matching HTTP response **after**
  the real route finishes, before the client receives it. `Wait(ctx)` observes
  that boundary; `Release()` delivers the original status, headers, and body.
  Check the durable state before treating arrival as a successful operation.
  This barrier is outside `routerMu`. Do not block an inner route handler:
  routing holds `routerMu.RLock` through `ServeHTTP`, so an inner gate would
  deadlock `FailoverControl`, which needs its write lock to retire the epoch.
- `StageBarrier(name)` creates a named container-stage barrier. Before starting
  a worker, call `SetStageScript(qualificationStageScript{...})`; the script is
  serialized into the subprocess configuration. Each phase accepts a `Barrier`
  name, an optional `Result` (`*apiv1.ResultEnvelope`), or an `Error` string.
  After release, an empty outcome continues the normal happy-path script.
  Phases are `parent-start`, `parent-granted` (live grant, before submission),
  `parent-submitted` (submission accepted; the production observer may now cancel the barrier to stop the parent),
  `parent-resumed` (after reading/verifying the receipt), and `child` (before
  the child stage returns). Ambiguous handoff failures stop the parent without
  a fresh dispatch; other errors follow the normal activity policy.
  Named barriers hold every visit, including worker retries, until released;
  scripts persist across worker replacement. Change scripts only while stopped.
  An already-created pod retains its original script: adopting it joins that
  execution instead of restarting phases or submitting a second child.
  Production parent dispatch and scripted child stages heartbeat immediately and every second, including while a
  barrier or HTTP response is held and during cancellation cleanup. The worker
  uses production's eight-second maximum heartbeat RPC throttle, so heartbeat
  replies deliver cancellation and the blocked request exits with it. A phase's
  explicit `HeartbeatLoss: true` suppresses heartbeat delivery (at a test-only SDK interceptor for production parent dispatch) until the next
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
On failure, harness cleanup dumps the original worker's output and every
restarted worker's output after joining them. Control and HTTP errors already
go to the test log. Scenarios do not need their own worker log dump.

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
// Assert durable binding and wait for Temporal to persist exact pod custody.
custody := h.WaitForPodCustody(run)
h.KillWorker()
barrier.Release()
h.StartWorker()
// Assert the scenario's expected workflow and custody outcome using run.
_ = custody
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

## Fault scenario boundaries

The retention, worker-loss, and failover scenario files add scenarios 2–4.
Their qualification still requires the hosted event evidence described above.
They use the shared stage barriers and outer HTTP response barrier;
all resumed requests and probes use the production authenticated HTTP planes.
Timestamp injection uses a separate database setup connection; runtime pools
keep their controller role and column grants.

- Retention ages only the acceptance and run-pin timestamps past
  `ReplayRetention` while the committed stage-origin acknowledgement is held,
  before production dispatch acquires its first grant. Production pruning and pin sweeping consult the real
  Temporal execution before the worker binds and completes its child.
- Worker loss kills and joins the worker after a grant binds. As designed in
  `engine-parent-child-authority.md` §5/§8, the grant remains usable while the
  parent is running and its TTL has not expired. One case ages only the retained
  grant expiry, leaving its signed bearer unexpired, to exercise the production
  database-clock refusal independently of parent liveness.
- Completion/replacement use a retained two-attempt parent definition and
  matching start pins. The scripted executor heartbeats while held, matching
  production's cancel-safe executor (#8230). Killing its process stops those
  heartbeats; Temporal's real heartbeat timeout drives production `Run`'s retry.
  Both cases assert that timeout in Temporal history. The replacement executor
  returns scripted success or follows the normal child path; no activity result
  is injected through Temporal's completion API. Kubernetes remains unqualified.
- Control failover holds the bind response after the grant row commits and
  before the worker receives the acknowledgement or submits a child.
  `FailoverControl` performs voluntary takeover. The successor reconstructs
  scheduler capacity, adopts custody, serves submission and idempotent replay,
  and completes the child/parent. Concurrent held scheduler reservations probe
  the bound of at most one extra admission per parent.
