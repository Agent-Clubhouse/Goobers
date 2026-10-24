# Fenced terminal journal interventions

> Status: implemented
> Tracking: #7899
> Delivered-by: #8111, #7899

Issue #7899 has two layers: the fenced file/PG journal seam (#8111), and the
versioned workflow/worker/API composition. A workflow advertises
`liveTerminalInterventions` only after recording
`hitl-live-terminal-transition-v1`. Older histories keep the original command
sequence and the API refuses terminal interventions on them before sending an
Update. Ordinary journal emission remains closed after `run.finished`.


## Authority and transport

`POST /api/v1/runs/{run}/journal/terminal-intervention` accepts only the dedicated
trusted engine-worker issuer. Both the route authorizer and handler enforce it,
including under permissive authorization middleware. Pods, generated children,
workflow parents, human admins, config workers and the other worker credentials
cannot invoke it. Its credential is never included in a stage invocation kit.

`SignedKey.MintWorkerTerminalIntervention` uses the existing kid current/previous
key ring, with the five-minute worker TTL bound. These are distinct MAC domains
(the final NUL byte is part of each domain):

| Worker authority | MAC domain |
| --- | --- |
| Config observability | `goobers/worker-config-digest/v1\x00` |
| Blob transport | `goobers/worker-blob/v1\x00` |
| Surrender reads | `goobers/worker-surrender-read/v1\x00` |
| Child admission | `goobers/worker-child-admission/v1\x00` |
| Terminal intervention | `goobers/worker-terminal-intervention/v1\x00` |

The non-wire MAC domain prevents changing a prefix to turn one credential into
another, including a pod bearer. Rotation tests retain old tokens while the
previous kid is enabled and refuse them after retirement. Route tests send real
signed pod and config-worker tokens and require 403 without a service call.

## Commit protocol

The request binds the run, request ID, payload fingerprint, expected terminal
sequence and expected terminal generation. The backend accepts only a keyed
`runner.annotation` marker plus the corresponding reopening event, or a single
keyed escalation-denial annotation. It uses the existing local intervention
marker vocabulary. There is no generic post-terminal append permission.

Every backend checks the current phase, generation, exact prefix and all emit
key collisions under its existing owner fence. A complete duplicate returns
its original receipt even after later segments finish, but a stale owner cannot
return success, including for a duplicate. Reuse of a key with changed payload
is a conflict. A new intent must target a failed or escalated terminal.

The initial segment is 1. After `g` committed `run.finished` events, reopening
enters segment `g + 1`; denial stays in segment `g`. Journal sequence numbers
remain global and monotonic. No separate segment counter or migration is needed.

PG uses the controller lease, owner epoch/token, and locked run counter. Its
marker, reopening event and checkpoint commit in one transaction. Immutable
history is folded before entering the bounded fence; only a concurrent tail is
folded inside it. The file backend holds the existing exclusive writer lock and
retains its ordered-prefix durability. If a crash commits only the marker, only
the identical intent may complete that prefix. Ordinary live emits stay closed.

The live writer retains a terminal handle only for this validation/commit. It
rehydrates the dedup state after the transition and clears the historical
terminal latch on resume, rerun or override. Borrowed file owners stay borrowed;
the transition neither retires nor closes their owner.

## Projection audit

Journal recovery, run phase, terminal-cause and readmodel reducers already reset
on `run.resumed`, `stage.rerun.requested` and `gate.overridden`. PG/file tests
compare exact event bytes and the resulting readmodel projection across two
segments. The portal's graph state and fallback failure banner had independent
historical terminal/error latches; both now reset at those same boundaries.
Historical sequence views still show their original terminal state.

PG16 regressions execute as `goobers_controller` and assert `current_user`.
They cover lost response across lease takeover, stale-owner duplicate refusal,
conflicting intents, denial, atomic rollback of a failed second append, resumed
live-writer recovery, and the ordinary post-terminal refusal. File tests also
cover recovering a partially committed prefix and preserving a borrowed owner.

## Cancellation reconciliation

A cancelled pending workflow Update uses the same fenced seam and binds the
original intent, generation and sequence. Its cancellation writes a keyed
`intervention.cancelled` annotation followed by `run.finished(aborted)`. It never
completes an uncommitted reopening prefix. If reopening already committed but
its reply was lost, cancellation aborts that resumed segment. An identical
cancellation returns its durable receipt; every late original retry is refused
with `ErrInterventionCancelled`.

The file backend can recover either a marker-only original intent or a
marker-only cancellation. A cancellation tombstone closes the ordinary emit
lane even when the prior resume committed. Only the exact cancellation may
finish that prefix. PG commits both cancellation events atomically under the
same owner fence. Ordinary emits cannot forge a cancellation tombstone; only
the dedicated worker credential can submit the bounded transition.

File and controller-role PG16 tests cover no prefix, original marker only,
fully committed reopening, cancellation response loss, and late retries. File
recovery additionally tests a crash after the cancellation marker. Workflow
retry state, operator cancellation routing and durable-before-ack handling are
the next layer; this seam does not change Update acceptance on its own.

Cancellation of a completed reopening is permitted only while the originating
Update has failed or remains delivery-uncertain and the workflow has not taken
up the resumed plan. It is reconciliation of that unacknowledged Update, not a
general abort operation for an already acknowledged/resumed run. PR-b must
retain that pending-intent binding until reconciliation completes.

Ordinary emits also refuse `intervention.idempotency`. The local intervention
service writes its marker with `journal.Recover`/`Append`; the legacy engine
still emits `escalation.resolution`, which stays allowed. For journals written
before this guard, `run.finished` clears pre-terminal pending annotations: they
cannot reserve an intervention against the newly committed terminal.

## Workflow delivery and acknowledgement

The API queries the workflow capability and terminal generation before routing
approve, override, rerun or deny. The payload-bound Update waits for the trusted
worker's journal activity to return its durable receipt. A lost response retries
the exact same keyed transition; it cannot acknowledge a marker held only in
workflow memory. A definitive generation, key or payload conflict refuses the
Update immediately. A missing dedicated worker authority, or a 401/403 from
the terminal route, is also a definitive refusal rather than an endless
transport retry.

Acknowledgement lookup retains the most recent 64 request-to-generation entries.
Before constructing an Update, the API resolves the caller's request key through
`goobers.hitl-request-state.v1`: a recent acknowledgement reuses its original
generation, an in-flight delivery keeps its reserved generation, and a previously
acknowledged key evicted from the window is refused as stale. An evicted key is
never rebound to the current generation or a later human-gate occurrence. The
existing workflow deduplication ledger identifies previously acknowledged keys;
it is not copied into the query response or a second unbounded generation map.
The request query is registered only for the new versioned capability, preserving
N-1 query and command behavior.

Query results are snapshots, not authorization. Under
`operator-request-key-ledger-v1`, the terminal-intervention and human-gate
handlers share one request-key/fingerprint ledger. Each stored fingerprint
includes its owning protocol. The validator refuses a key owned by the other
protocol, and the handler checks again after acquiring its mutex, then claims
without yielding before journal delivery or gate consumption. This prevents an
in-flight duplicate whose lookup preceded the original acknowledgement from
approving a later same-name human gate. The inverse cross-protocol reuse is
also refused with the protocol's existing typed key-reuse error. Pending claims
are protected too. The shared map replaces the separate fingerprint maps; it
does not add another unbounded key index. N-1 histories retain their original
independent ledgers and replay unchanged.

Only one terminal delivery can be pending. The workflow holds terminal settlement
while it is pending, even after the original operator hold deadline. The activity
uses server-side retries with exponential backoff capped at 30 seconds, so retries
do not add workflow commands. `enginedecision.Router.HITLState` includes
`intervention pending (journal delivery retrying)`, its request ID, attempt count
and last error, using Temporal's pending-activity record for retry evidence.

A cancelled pending Update is reconciled through the same worker-only seam on a
disconnected context before ordinary abort emission. The returned committed-prefix
length determines exactly which events enter the workflow's repair projection.
An uncommitted intent is tombstoned; an already committed prefix is aborted. Late
retries cannot reopen it. This is only for an Update whose delivery remains
uncertain; it is not a way to cancel an acknowledged, consumed transition.

Human task reruns keep the cumulative attempt number and `human` class while
starting a fresh per-invocation retry budget. Explicit reviewer reruns clear the
previous diff shortcut and require the reviewer to run even when automation would
skip an empty diff, matching the local runner. Both changes are reachable only
through the new versioned, committed terminal transition. The caller's raw
request determines the shared idempotency fingerprint; an omitted approval
verdict defaults to `pass` for branch selection and `run.resumed` only.

## Workflow qualification

The restored runner/engine parity row covers override, task rerun and denial,
plus default approval and reviewer rerun, using the production activity and
workspace adapters. Real Temporal histories exercise all four terminal verbs,
full replay and every workflow-task prefix. A captured N-1 live-journal history
pins the old path. Lost-response tests restore a completely unavailable response
path, retry the same UpdateID and verify one durable reopen and one journal
activity command. PG16 tests run as `goobers_controller`, assert `current_user`,
check the durable marker at acknowledgement, and cover cancellation followed by
a late delivery without any local journal files.
