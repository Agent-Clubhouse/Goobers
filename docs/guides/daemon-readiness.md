# Daemon readiness and trigger progress

A listening HTTP API does not mean startup has finished or that the daemon is
currently consuming trigger requests. Query `GET /readyz` to distinguish them.
This unauthenticated probe returns only readiness booleans, not configuration,
run details, or credentials.

The response's `checks` includes:

- `apiListening`: the daemon has successfully bound its API listener.
- `schedulerReady`: startup admission is open and the scheduler has reported
  progress within the configured scheduler-liveness timeout.
- `triggerSweepReady`: startup admission is open and a trigger sweep completed
  successfully within that timeout. Failed sweeps do not refresh this check.
- `configLoaded`, `stateOpen`, `resumeComplete`, and `sweepsStarted`: the
  existing startup milestones.

During a long startup, `apiListening` can be true while `schedulerReady` and
`triggerSweepReady` remain false. After startup, a stalled trigger sweep can make
`triggerSweepReady` false even when the scheduler itself remains responsive.
The checks use in-memory timestamps so a stalled filesystem cannot block this
diagnostic request.

The top-level `ready` field and HTTP status retain their existing startup and
shutdown admission semantics: HTTP 200 when that gate is open, otherwise 503.
They are not recomputed from the diagnostic checks. Clients diagnosing trigger
delays should inspect `schedulerReady` and `triggerSweepReady`, not infer trigger
progress from HTTP 200 alone. `/healthz` separately reports process/scheduler
liveness, including the existing grace period before the first scheduler tick.

No health probe is proof that an individual trigger was accepted or dispatched.
Use the trigger submission's own result for that decision; do not automatically
resubmit a request merely because a progress check becomes stale.

`POST /api/v1/triggers` requires an `Idempotency-Key` header (up to 256 bytes,
with no embedded control characters). Reuse the same key and payload when
retrying a delivery. If the JSON body includes `requestId`, it must match the
header after trimming surrounding whitespace; the header alone is sufficient.
The remote `goobers run` and pod priority-trigger clients transmit both values.
The daemon commits acceptance to `accepted-triggers.db` before returning HTTP
202 with `acceptanceId` and `state`. Acceptance can precede scheduler readiness;
it is not a promise that run admission will succeed. Retrying the same key and
payload returns the same acceptance identity and the recorded dispatch state.
`goobers run --no-wait` succeeds on that acceptance.

The daemon derives one run ID from the durable acceptance identity and passes
it through manual and priority dispatch. Reopening the queue does not allocate
a different run ID. This is the identity boundary for crash reconciliation,
not permission to blindly restart an already dispatched run.

The queue retains at most 10,000 records and refuses new requests when all slots
remain occupied. Only terminal records older than seven days can be pruned on
insertion; pending requests and uncertain dispatches retain their slots. After
a crash, the daemon reconciles `dispatching` records against matching published
run journals. Scheduler admission alone does not finish a record: the starter
runs asynchronously, so `dispatching` can include an assigned `runId` before its
journal is published. Missing or inconsistent journals remain unfinished and
are not automatically replayed. Recovery of those absent-run cases is still
pending in this implementation branch.

Query `GET /api/v1/triggers/{acceptanceId}` with the accepting identity's bearer
to read `state`, `acceptedAt`, and any `runId` or refusal `reason`. This lookup
does not submit or dispatch a trigger. Other identities receive the same 404 as
an unknown ID. Pod callers need their state-scoped bearer and can read only
acceptances recorded for their own pod run. Status responses are not cached.
