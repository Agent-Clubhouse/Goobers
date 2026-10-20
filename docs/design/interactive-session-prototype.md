# Interactive session coordinator prototype

> Status: draft — working experimental branch implementation; not shipped or accepted as the production design.
> Area: interactive sessions

This document describes the experimental `internal/interactive` implementation,
not a deployed cloud service. Ordinary declarative workflows retain their existing
scheduler, approval, and completion behavior.

See the [launch and qualification guide](../guides/interactive-prototype.md)
for the integrated portal, executor and delegated-access checkpoint.

## Configuration and execution state

Interactive configuration is first-class `Gaggle.spec.interactive`, validated
and loaded through the normal Manifest/Gaggle/Goober/Workflow configuration path.
An optional Gaggle-level `dslVersion: "3.0"` pins this additive contract only.
The Gaggle owns its interactive preview acknowledgement. Existing fields still
use the original workflow-derived feature checks, and every Workflow keeps its
own version/preview acknowledgement. No surface is created when the block is
absent or empty. The experimental command does not use a parallel surfaces JSON
format and is not wired into the production daemon.

A surface selects `helper` or `workbench`, binds an existing local/shared Goober,
and declares execution modes, mediated tools, same-gaggle workflow names, local
broker-reference names, session ceilings and child command capabilities. Goober
harness/model/Markdown instructions provide the agent binding. Omitted model
retains the existing harness default. The first runtime adapter supports Codex
only and rejects unsupported Goober options explicitly. The surface's mediated
tool list is distinct from Goober builtin-tool policy; it cannot constrain a
trusted native harness's shell or filesystem tools.

The host takes a private immutable snapshot of normal config files and adjacent
instance-shared Goober definitions/instructions, then validates that snapshot
with `instance.LoadConfigDir`. Resolved contents participate in the revision,
including Markdown. Manifest selection controls active Gaggles; runtime surface
IDs include the gaggle name. Authority declarations contain opaque non-secret
host broker references, not connection syntax, tokens or new grants.

A private session pins owner/membership, surface configuration digest and config
revision, immutable `executionMode`, turns, review children, cumulative workspace
changes, leases, commands and ordered events. `codex-local` and `local-tools`
are explicit modes. Available modes are the intersection of DSL policy and host
permissions. The sample requests Codex by default; a host without its trusted
local opt-in rejects that request. There is no fallback to the deterministic demo.

`POST /api/v1/interactive/config/reload` validates a new snapshot before replacing
the catalog. Invalid input preserves the last known-good policy. A valid changed
revision fences the old sessions, cancels active execution and preserves custody
reservations until writer stop is verified. A new session is required for the new
revision. This also applies to instruction changes, removed/disabled surfaces,
and reduced policy. History, stop and final child review remain available.
Current-policy checks belong in execution admission and renewal as well as human
commands, so an already running agent cannot keep admitting old-policy tools.
There is no silent migration of grants or automatic source-directory watcher.

The coordinator persists the entire bounded prototype ledger as a mode-0600
JSON snapshot. Each transaction writes and fsyncs a temporary file, renames it,
and syncs the containing directory. Failed or uncertain persistence freezes new
admission and cancels workers. A process-level OS lock prevents two coordinators
from owning the same state directory. This implementation is **one coordinator
per store**, with multiple isolated owners and bounded concurrent execution. It
is not a horizontally scalable distributed coordinator or a cloud scheduler.

Sessions themselves require no execution process while idle or awaiting auth.
The embedding host caps interactive execution at four active leases globally
and two per owner; declared per-surface ceilings may narrow those bounds. Native
workflow workers have a separate four/two pool. These are local prototype limits,
not cloud capacity guarantees. Children consume admission capacity. A
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

## Model tools and child boundary

`internal/interactiveagent` supplies an invocation-bound native MCP bridge over
a private Unix socket/stdio proxy. The host advertises only the surface's allowed
subset of seven typed tools: `session_context`, `inspect_action`,
`validate_workflow`, `propose_action`, `propose_child_workflow`, `read_work_item`
and `query_telemetry`. The host binds actor/session/turn/lease/fence and checks
current configuration before dispatch. Tool arguments cannot select those
identities, inject credentials or approve an effect. Human approval is a separate
portal control. Socket/client disconnect does not prove that an admitted effect
failed; exact host-owned intent IDs and durable receipts govern reconciliation.

`propose_child_workflow` validates authored bounded Workflow YAML, then calls
`RequestChildFromExecution` with the trusted invocation identity. The coordinator
checks the exact current parent lease and records the stable intent atomically.
The same logical key/payload replays its receipt; a changed payload conflicts.
A stale lease cannot make a proposal. A model cannot choose an owner, invoke a
child recursively or approve its own proposal.

A child with exact YAML has `executionKind: interactive-workflow`; an optional
semantic review without YAML retains `interactive-review`. The proposal stores
the immutable definition and its digest. Changed work requires a new proposal.
It can be proposed during a parent turn, but approval requires an idle parent
checkpoint. Parent writes wait for active children because this prototype has
no atomic snapshot barrier. A child has a separate identity, lease, attenuated
authority and independent workspace; `ReadOnly: true` means no adoption into or
writing of the parent's workspace. Results require explicit review.

The bounded workflow adapter admits manual DSL 2.0/3.0 workflows with 1–8
acyclic deterministic stages, 1–30 second stage deadlines, scratch execution,
explicit scoped Node test/check paths and the appropriate no-network declaration.
DSL 3.0 requires its own preview acknowledgement and `runsOn.restrictions`;
DSL 2.0 uses `run.network`. Host path binding and configured command capabilities
are checked before native execution. The adapter records native scheduler/run
receipts and observes journal completion; a queued admission is not success.
The deterministic demo uses a bounded test workflow. Native semantic review is
part of the unqualified model path, not a claim supported by those test runs.

This uses actual ordinary native workflows but does not integrate the production
`internal/childworkflow` lineage/admission model or DSL 3.1 `task.childWorkflows`.
That path requires an opted-in agentic parent task and its production catalog and
runner context. The prototype does not fabricate that parent or establish
arbitrary generated workflows, publication, recursion or distributed scheduling.

## HTTP and output boundaries

The handler is mounted at `/api/v1/interactive` and requires an injected
principal resolver. It never accepts a request-body owner. All session,
artifact, replay, and command reads enforce the private owner, including
reauthorization during SSE. The integrated app supplies clearly labeled controlled
local personas; that is not proof of remote portal authentication. Its loopback
configuration reload and provider-fixture controls are host-level prototype APIs.

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

## Verification boundaries

`GOCACHE=/tmp/goobers-interactive-cache go test -race ./internal/interactive`
passes deterministic fake-executor lifecycle tests and actual HTTP handler/SSE
transport tests. These cover owner isolation, create/command replay, conflicting
payloads, stale sequences/fences/leases, admission budgets, competing coordinator
locks, restart refusal and recovery, child review/limits, steering receipts,
auth waits, stop propagation failure, orphan custody, partial edits, and split
stream redaction. They do not prove native subprocess custody, live Azure/ADO
authentication, or live provider effect behavior; those require the assembled
adapter and separate qualification evidence.

Normal DSL tests also cover the closed schema, bounded policy, local references,
foreign-gaggle rejection, independent preview pins, workflow-less Gaggles,
`instance.LoadConfigDir`, CRD installability, deep-copy isolation and normal
configuration generation apply/replay/update. Kubernetes transport in the apply
test is fake. Native MCP transport tests use actual local sockets with controlled
callbacks; model adapter tests use a substituted harness. These are distinct from
real local filesystem/Node/native-workflow integration evidence.

The current compiler/reload/model-tool integration requires a final assembled
qualification record; prior browser counts do not qualify later code. Live model
execution remains unqualified: automatic approval review denied the attempted
sample disclosure to the model service before execution, pending specific user
approval. Live Azure/ADO sign-in, token acquisition, resource authorization,
production daemon integration and distributed coordination are not implemented
by this checkpoint. Native Azure CLI or token-consuming MCP compatibility cannot
be inferred from mediated broker tests.
