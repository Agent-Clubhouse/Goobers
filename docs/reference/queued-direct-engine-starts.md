# Queued direct engine starts

`goobers engine-start --direct` durably records its exact execution input before
contacting Temporal. Direct starts retain the existing behavior: their run ID
comes from gaggle, workflow and dedupe key; they bypass scheduler concurrency
admission and terminal hooks. When no daemon is present, engine-start still uses
the direct path. Daemon-delegated starts use the separate file-request adapter.

## Acceptance and replay

The shared trigger ledger stores a bounded canonical input attachment atomically
with its receipt. It captures configuration generation, execution options, exact
frontend, namespace and task queue. The credential binding stores a digest of
configured TLS/codec selectors; credentials are resolved through their existing
providers when the matching transport is opened.

An exact retry reuses the first committed input, including when concurrent
callers capture different generations. Changed options or credential selectors
conflict. Specify `--gaggle` when retrying after the authored definitions have
changed or become invalid: an unqualified invocation still needs those definitions
to discover its target. A new acceptance always compiles and validates its input.

## Remote uncertainty

The receipt records an attempted dispatch before any start call. If the response
is lost, a restart or retry observes the exact Temporal execution; it never
resends an attempted start because history is absent. Confirmation verifies the
actual first history event, workflow type, task queue, complete input digest and,
when present, the acceptance memo. Missing, unreadable or mismatched history
retains uncertainty instead of reporting success or starting another run.

The daemon examines at most one direct receipt per sweep with a bounded network
context. Direct receipts have their own cursor and do not occupy the ordinary
scheduler's pending or uncertain batches. Accepted and uncertain inputs retain
their configuration generation. Confirmed remote histories keep the existing
external generation ownership marker.

## Storage and incremental scope

An additive queue migration attaches input to its receipt in the existing
private database. Inputs are limited to 1 MiB and share existing database byte
and receipt limits. Removing an expired terminal receipt also removes its input;
accepted and uncertain receipts are retained. This version is required to read
and dispatch the newly migrated queue.

This LAND-E01 slice adapts direct-engine portions of reference snapshot
`fa34a754148ea3076bfd04fe4976a5a461b063f2` onto the existing queue and generation
owners. It excludes the reference's future session, event and start-control
systems. Schedules/demand, signals and interactive restart adapters remain
separate work.

Validation covers actual CLI acceptance and replay, independently opened database
writers, prior-schema migration, changed credential bindings, mismatched history,
and a disposable real Temporal frontend with a discarded successful start reply.
The real-frontend test confirms acceptance and recovery; it does not qualify
execution of the workflow's stages on worker pods.

## Key-store platform boundary

The existing `file-key` backend requires POSIX-private directory and file modes.
It cannot prove those modes on Windows and refuses key operations; it does not
fall back to plaintext. A queued start encountering that refusal retains its
uncertain receipt until exact provider history can prove the outcome. Default
codec starts and history reconciliation run on every supported platform. This
adapter does not add Windows ACL support to `file-key`.
