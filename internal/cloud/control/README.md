# Cloud control role (HA-P3-18)

`goobers-cloud control` combines the authenticated HTTP planes with a
single-active controller. It does not register a Temporal worker or execute a
stage. The remote engine task queue is served independently.

The lease manager renews every 5 seconds, expires after 15 seconds, and fences
locally after 10 seconds without a successful renewal. Standbys retry every
2 seconds. Schema compatibility is checked before acquisition and while active.
Every epoch rebuilds claims against open workflows, reconciles uncertain starts,
and reattaches awaiters before readiness or any L1/L2/L3 tick. Losing authority
cancels the epoch. SIGTERM cancels loops, stops epoch services, and voluntarily
releases the lease; it does not cancel Temporal workflows. If callbacks fail to
stop within the shutdown bound, shutdown exits non-zero without releasing early;
renewal stops and the database TTL expires.

BeginDispatch is the provisional reservation. Starts use only the durable row's
run ID and are confirmed/adopted after Temporal classification. A new owner gets
one immediate attempt after a dispatch owner loses authority; later epochs retain
backoff and remain visible as `start ownership uncertain`. Attempts are bounded
and independent, so one uncertain workflow cannot stop healthy admission.
Provider observations are bounded JSON snapshots, restored before rebuilding
and fenced before admission. Provider adapters supply versioned, non-secret
quota/MaxOpenPRs observations; snapshots never authorize execution themselves.

## Required composition seams

The command's `assembleControlPlanes` must receive the independently landing PG
read, claims, trigger, journal-write and journal-read services, pinned generation
resolver, and epoch service factory. The factory supplies rebuild-before-reap,
uncertain reconciliation, await reattachment, policy/renewal/config ticks,
provider observation capture/restore and shutdown. Each mutable backend must
bind the exact acquired handle; it must not borrow a newer global owner from an
old goroutine. Callbacks must honor cancellation. A failed builder returns any
allocated Stop callback so the host can clean up.

The assembly also receives signed pod/grant minting keys, credential-grant
verification and durable revocation. Key reload runs for the process lifetime;
malformed replacement key rings preserve the last good keys and report errors.
The default human authenticator denies access pending the cloud identity
adapter. Status requires an instance-view principal; pod/grant identities cannot
read global status. Health/readiness probes carry no private workflow data.

Until those PG/lifecycle seams are assembled, the command exits with an error
before dialing Temporal or acquiring the controller lease. Dynamic preparation
failures use exponential contention backoff, capped at one minute. There is no local filesystem, SQLite, registry-auth
or in-process runner fallback, and no fabricated successful recovery callback.
The actual dependency assembly is deliberately reviewable in the command.

### Human-gate composition checklist

After [#7897](https://github.com/Agent-Clubhouse/Goobers/pull/7897), production
engine `Run` can execute human gates. Engine support alone does not make cloud
admission safe: every admitted gate needs an authenticated decision route.
The control/API composition in #7850 and #7879 must complete these steps in order:

- Construct `enginedecision.Router` with the Temporal client and install it as
  `intervention.Config.EngineDecisions` in the served intervention plane.
- Verify that authenticated decisions reach the same Temporal workflow that
  cloud admission starts, including after controller lease turnover.
- Only then allow human-gated runs through cloud admission. Until the router
  and client are wired, keep a cloud admission refusal for human gates even if
  the engine runtime-support column permits them. A wired standalone `api`
  role does not establish that the combined `control` role is wired.

## Validation

`GOOBERS_TEST_POSTGRES_URL` points only to a local PG16 container. Run:

```
go test -race -tags cloudintegration ./internal/cloud/control ./internal/cloud/acceptance
```

The control suite uses a real Temporal dev server with no worker. It seeds a
start through the first epoch's fenced starter captured at the mandatory
Admission seam, hides Temporal's successful response, then transfers the lease
between two replicas. It verifies successor recovery to Dispatched, exactly one
Temporal start, restored observations, and a workflow still Running after both
controllers stop, and measures in-process handoff latency. The controller
composition suite separately exercises real scheduler admission and refusal handling.
A second variant injects heartbeat renewal failures, drives the monotonic self-fence deadline,
and requires real database TTL expiry before involuntary takeover. Live callback
accounting checks overlap independently of Stop. The deadline test checks
ErrSelfFenced using a controlled clock, without wall-clock timing assertions.
The HA nightly control-restart job runs 20 samples with production lease timings
and reports in-process voluntary handoff p95 against 60 seconds. This is not the
HA-P3-18 kind rollout RESTART-RTO gate, which remains pending. It isolates PG/Temporal
acceptance recovery; full provider/claim rebuild awaits the required composition
seams. A single test's elapsed time is not a production percentile.

The PostgreSQL write-path inventory is the authority for the two pools. API
revocations are immutable INSERTs without a controller lease; expiry cleanup is
controller-fenced DELETE. Acceptance retention pruning also runs only on the
controller pool. Migration grant reconciliation removes the previous API DELETE
and controller revocation INSERT grants. PG16 tests use the shared role provisioner
and the real inventory, including a rolling-upgrade grant removal regression.

Cloud webhook intake (#7914, `internal/cloud/webhookintake`) is mounted by the
combined role at `/webhooks/github` when `--github-webhook-secret-file` is set.
The delivery HMAC is verified before any durable write. The API pool, connected
as `goobers_api` with no controller credentials, records one source receipt per
`X-GitHub-Delivery` and its recipient acceptances in a single transaction
serialised on the delivery key, so concurrent redelivery to different replicas
returns the same receipt and acceptance IDs. Workflow digest pinning
(`webhookPin`) arrives with the start-input resolver composition; until then
intake answers 503 and records nothing. CHAOS-9 stays pending until a hosted
nightly runs it against a deployed stack.
