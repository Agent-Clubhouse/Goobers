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
Kubernetes pod. Worker stage-activity registrations are the only executor
substitution. This is hosted backend qualification, not real Kubernetes
qualification (scenario 10). Control uses the runtime controller database role;
the worker subprocess receives only the Temporal address, HTTP endpoint, and
signing-key file, with no PostgreSQL or Azure credentials. All persistent custody
and journal operations use the production HTTP routes and backend adapters.
The scheduler is the production `localscheduler`/`SchedulerCapacity`; the fixture
reserves the parent's slot after starting it rather than running trigger polling.

The production config-load refusal and default remain unchanged. The harness
loads a normal fixture config and enables children only on its in-memory instance
config and the parent's pinned `RunInput`. Retained generation contents still pass
normal validation. No process-wide switch or production bypass is installed.

## Adding scenarios

Add a `qualification_<scenario>_integration_test.go` in package `childengine`:

- `newQualificationHarness(t)` creates an isolated database, blob container,
  Temporal server, control HTTP server, configuration, and scheduler.
- `StartParent()` confirms the start pins, reserves capacity, starts a separate
  worker, and returns the real Temporal `client.WorkflowRun`.
- `KillWorker()` kills and joins only the harness-owned worker PID;
  `StartWorker()` replaces it using the same task queue and durable services.
- `FailoverControl()` closes the old HTTP/journal owner, releases the lease, and
  composes a successor epoch. Scheduler capacity is reconstructed from Temporal
  liveness and committed child-wait journal events. This models voluntary
  takeover; a scenario testing lease expiry must inject that fault explicitly.
- `ResetParent(run, workflowTaskCompletedEventID)` issues the Temporal reset RPC.
- `ReloadConfig(edit)` edits the fixture config directory, records a new archive,
  and atomically changes the applied candidate. Old inputs and archives remain
  pinned. It tests generation replacement; it does not run the directory watcher.
- `SetSlotLimit(n)` reloads scheduler capacity without changing run inputs.

The fields expose the real backend/client for assertions and narrowly scoped
fault injection. Scenario tests must synchronize on an observed durable boundary,
not a guessed delay. Keep the control/worker implementations shared; do not copy
or replace custody with mocks. Cleanup joins the worker and lease renewal,
closes HTTP/journal resources, stops Temporal, and deletes the test blob container
and database. A missing dependency fails with `TESTDEP_STRICT=1` in hosted CI.

Before declaring a scenario qualified, read its passing event (including any
subtests) from the hosted `cloud-pg-test-events` artifact. A compile-only check or
skipped local test is not qualification evidence. Scenarios 2–9 and the kind
nightly remain unqualified by this happy-path test.
