# Interactive session coordinator prototype

This document describes the experimental `internal/interactive` implementation,
not a deployed cloud service. Ordinary declarative workflows retain their existing
scheduler, approval, and completion behavior.

## Execution and state

An explicitly configured surface selects `helper` or `workbench`. A private
session records its owner, an owner membership entry, its surface configuration
digest, turns, bounded review children, cumulative workspace changes, leases,
commands, and ordered events. Changing a surface's definition prevents new work
in existing sessions. Stop and final child review remain available. This
prototype intentionally refuses migration rather than silently expanding a grant.

The coordinator persists the entire bounded prototype ledger as a mode-0600
JSON snapshot. Each transaction writes and fsyncs a temporary file, renames it,
and syncs the containing directory. Failed or uncertain persistence freezes new
admission and cancels workers. A process-level OS lock prevents two coordinators
from owning the same state directory. This implementation is **one coordinator
per store**, with multiple isolated owners and bounded concurrent execution. It
is not a horizontally scalable distributed coordinator or a cloud scheduler.

Sessions themselves require no execution process while idle or awaiting auth.
Admission defaults to four active executions globally and two per owner,
configurable by the embedding command. Children consume the same capacity. A
surface's child count is a lifetime reservation; denying or completing a child
does not replenish it. Each execution also has a bounded wall-clock deadline.
Retention limits refuse new work instead of deleting dedupe or effect evidence.
Executor event output has an eight MiB per-session default budget, independent
of command receipts and terminal custody records, so noisy tools cannot consume
that ledger space. Operators can lower this budget for constrained environments.
The full-snapshot store is intentionally limited to this prototype; large
production histories need a transactional indexed ledger and retention policy.

## Replay, leases, and recovery

Create and command IDs are stable idempotency keys. Reusing a key with different
content conflicts. Replay is resolved before checking current sequence/fence,
so a lost HTTP response never becomes a second execution. New commands require
the current event sequence and session fence. Reconnect only reads/replays; it
never starts work.

Each admitted execution receives a unique lease ID, worker ID, expiry, and
session fence. The executor must renew its lease periodically and stop when
renewal fails. A new ordinary turn preserves the session authority fence but
receives a different lease. Both event publication and provider admission check
the exact lease; an old turn cannot write into a new turn. The coordinator's
lease sweep advances the fence, cancels current workers, and moves the session
to `recovery_required`. Capacity remains reserved until the executor confirms
that its physical writers have stopped.

Restart marks active attempts interrupted and advances their fences. Interrupted
children become uncertain; no turn or child is replayed automatically. Explicit
resume requires `RecoveryExecutor.Recover` to confirm old writer custody and
reconcile ambiguous effects. An executor without that capability cannot resume.
Likewise, stopping an interrupted session does not report `writersStopped` while
orphan custody remains unverified. `custodyPending` describes that wait. An executor that cannot verify workspace
quiescence returns `ExecutionResult.CustodyPending`; the coordinator retains a
`custody_pending` lease reservation, advances the fence, and requires recovery
instead of treating process return as proof of custody. A stop request remains
recorded and cannot be converted into resumed editing. Only successful custody
verification permits terminal stop or a new eligible attempt.

Stop first persists the session fence. It then propagates broker revocation
through `OnStop`, outside the session mutex so final broker admission does not
invert locks. The callback's failure leaves `stopFencePending` and a truthful
`stopping` state; another stop retries propagation. Terminal `stopped` requires
both fence propagation and confirmed writer return. The stopped writer's final
workspace snapshot is retained, including partial edits, without claiming turn
success. Disconnection has no stop effect; the admitted bounded execution
continues until completion, expiry, or explicit stop.

The broker uses `CheckExecution(owner, sessionID, turnID, leaseID, fence)` for a
final current-lease check. This is an admission boundary, not an atomic
transaction with an external provider. An operation admitted before a concurrent
fence may still dispatch or finish afterward; its durable operation receipt must
be reconciled by the broker. Session history must never substitute for the
authoritative provider operation ledger.

## Steering and auth waits

Steering is accepted durably for one exact turn/fence, queued to that executor,
and reported as delivered only after its acknowledgement callback succeeds.
Completion, interruption, or restart rejects steering that lacks a confirmed
delivery. A child's completion cannot consume or reject its parent's steering.
Adapters that cannot steer should acknowledge an explicit error rather than
claiming delivery.

An auth wait is not completion. The executor returns `WaitingAuth`, the session
retains the active turn reference, and its compute lease is released. Explicit
resume creates a fresh attempt and requires the broker to revalidate connection,
authority, decisions, resource bounds, and ambiguous operations. Reauth cannot
renew a deadline, reset the shared budget, revive revoked authority, or fall back
to application identity.

## Child boundary

The current child is a dynamically requested, explicitly reviewed,
`executionKind: interactive-review` operation. It runs through the same bounded
executor interface with `ReadOnly: true`, a separate lease, and a separate
workspace supplied by the execution adapter. A proposal can be created during
a parent turn, but approval requires an idle parent. New parent turns wait for
active children to finish, because this first adapter has no atomic snapshot
barrier; `checkpoint_required` explains either refusal. A completed result requires an
explicit `accept` review. No patch is automatically applied to the parent.
Children cannot recursively invoke the session command API or create grants.

This adapter is not an existing generated declarative workflow run. The
`internal/childworkflow` validator requires a pinned instance/gaggle catalog,
opted-in agentic parent task, approved Goober set, and runner/engine backend.
Inventing a synthetic parent would misrepresent those checks. That production
validator remains unchanged. A future authored-workflow child path must invoke
it with real pinned configuration, then obtain an actual scheduler admission
receipt. The prototype's limited review child must not be described as proving
arbitrary generated workflows, publication, or distributed child scheduling.

## HTTP and output boundaries

The handler is mounted at `/api/v1/interactive` and requires an injected
principal resolver. It never accepts a request-body owner. All session,
artifact, replay, and command reads enforce the private owner, including
reauthorization during SSE. The prototype app may supply a clearly labelled
loopback identity; that is not proof of remote portal authentication.

Routes are `GET /surfaces`, `GET|POST /sessions`,
`GET /sessions/{id}`, `POST /sessions/{id}/commands`,
`GET /sessions/{id}/changes`, and `GET /sessions/{id}/events?after=N`.
An `Accept: text/event-stream` header selects SSE, with durable sequence IDs and
`event: interactive`; JSON replay is also available. Commands include `message`,
`steer`, `stop`, `resume`, `child.request`, and `child.review`. Every command has
`commandId`, `expectedSequence`, and `expectedFence`.

Credential custody belongs in the broker. Executors must keep tokens out of
prompts, tool output, workspace files, and artifacts. The coordinator additionally
copies and scrubs configured instructions, normalizes typed results, and
scrubs recognized secret shapes and sensitive field names, buffering assistant
and tool deltas across chunk boundaries. This pattern defense cannot detect all
unknown secrets, especially bare ADO PATs; it does not make arbitrary native
credential exposure safe. Native CLI/MCP access and mediation remain distinct
capabilities of the embedding adapter.

## Executed verification

`GOCACHE=/tmp/goobers-interactive-cache go test -race ./internal/interactive`
passes deterministic fake-executor lifecycle tests and actual HTTP handler/SSE
transport tests. These cover owner isolation, create/command replay, conflicting
payloads, stale sequences/fences/leases, admission budgets, competing coordinator
locks, restart refusal and recovery, child review/limits, steering receipts,
auth waits, stop propagation failure, orphan custody, partial edits, and split
stream redaction. They do not prove native subprocess custody, live Azure/ADO
authentication, or live provider effect behavior; those require the assembled
adapter and separate qualification evidence.
