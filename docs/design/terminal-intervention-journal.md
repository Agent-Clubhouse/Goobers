# Fenced terminal journal interventions

Issue #7899 is delivered in two layers. This layer provides the durable journal
transition and its worker-only transport. The workflow/API layer adds the
Temporal version marker, terminal capability discovery, and durable-before-ack
Update handling. Until that layer is composed, the existing terminal HTTP
intervention refusal remains in place.

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
