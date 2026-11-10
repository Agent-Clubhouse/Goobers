# Design: Child workflows and conditional fan-out on the Temporal engine

> Status: draft — implementation and qualification required
> Area: distributed engine, admission, journal, workspaces, capacity
> Related: #155, #1310

## 1. Decision and scope

Make the distributed Temporal engine walk (mode 3) a first-class executor of
agent-authored child workflows, then of static and conditional fan-out. The cloud
engine is an HA, multi-tenant, auditable service that survives rolling upgrades.
Scheduling a stage must durably bind its identity, authority, and execution intent;
a worker crash must not leave an unowned dispatch or silently duplicate effects.

This proposal **reverses decision 005 R9's “not being ported” position for
`task.childWorkflows`, `spec.parallels`, `spec.parallels.selection`, and `spec.maps`**.
It does not reverse other R9 refusals. The current refusal behavior stays in place
until each supported slice and its recovery tests land. This document proposes
direction; it does not claim that the reversal or implementation has been approved.

Phase 1 supports generated task/gate graphs, with one active path through the
graph and the engine's existing automated and agentic gates. Human gates remain
refused. Parents also use this serial graph subset. Existing bounded gate repasses
remain valid; “linear” does not require removing ordinary gate branching.
Phase 2 adds engine static parallels, selective parallels, and maps, including
composition in generated children. Recursive child generation remains refused.
Neither phase changes DSL lifecycle levels or promotes preview features to GA.
Phase 1 does not enable human restart of a sealed child: retain its result and
expose the unsupported operation explicitly. The existing execution-epoch
contract remains reserved for a separately qualified restart adapter.

The approved [HA engine-parity decision](decisions/ha-engine-parity.md) broadens
the R9 reversal to all eight refused feature categories and amends delivery
order: standalone parallels, selection, and maps now precede child workflows,
based on shipped usage. The phase numbering in this draft still describes
composition dependencies within the child-workflow design; it no longer sets
the programme-wide P4 implementation order.

The [agent-authored child contract](agent-authored-child-workflows.md) governs
authority, workspace custody, result disposition, and one unfinished child per
stage occurrence. The [fan-out contract](plan-driven-dynamic-fan-out.md), especially
§13 and §15, describes today's runner-only delivery; this proposal adds a separate
engine delivery track and preserves its source-freezing and join semantics.

## 2. Current gaps

`internal/engine/registryrefusal.go` refuses all four constructs at start;
`engine.go:run` also calls `RefuseChildWorkflowExecution` and
`RefuseUnsupportedFanOut`. `RefuseDefinition` influences scheduler placement, so
changing only `run` would still leave ordinary starts on the runner. Conversely,
removing a registry refusal alone would advertise a feature the walk cannot run.

`parity_test.go` excludes `ChildWorkflowOrigin` because an engine projection's
ordinal is not a committed journal sequence. `emitPending` receives a live-journal
response but currently advances its watermark without carrying a committed child
origin into dispatch. The shared `JournalAuthorityResolver` verifies an actual
active stage event and pinned policy; a guessed origin cannot satisfy that contract.

The contained child pod transport already uses Temporal activities, but that is
not an agent-authored graph executed as a Temporal child workflow. Existing
`ChildDispatchInput` and pod recovery must not be mistaken for this graph protocol.
The runner's handoff, cancellation, snapshot, and capacity code supplies semantics
to reuse, not an in-process runner to embed inside the engine workflow.

## 3. Frozen definitions and wire contracts

Admission retains exact proposal bytes under `SourceDigest`, the canonical
proposal digest, and the compiled workflow digest as distinct identities.
Formatting changes may preserve canonical identity but cannot replace an accepted
source receipt. Freeze the compiled graph together with DSL/interpreter contract
version, configuration generation, Goober kit digest, placement decisions,
effective grants, repository bindings, and parent policy digest. Store an immutable
versioned manifest of these pins by content digest. A mutable registry name is
never enough to load a generated definition.

The proposed generated-child extension to `RunInput` carries:

| Field group | Meaning |
| --- | --- |
| Definition reference | Manifest digest, exact source digest, compiled workflow digest, interpreter version |
| Lineage | Tenant/gaggle, root run, parent run, committed stage occurrence, initiating attempt, invocation sequence, child acceptance ID, execution epoch |
| Custody references | Parent snapshot, fork manifest, bounded continuation context references |
| Execution policy | Explicit preview acknowledgement, live-journal requirement, supported engine protocol version, deadline and budget pins |

Use the existing child identity encoding, scoped to tenant/gaggle, to derive a
stable Temporal Workflow ID for each accepted child execution epoch. Keep Temporal
Run ID separate: it identifies a Temporal execution, not the business child.
Retries retain identity; stage revisits and sequential child requests get new
occurrence/sequence identities. The start policy rejects duplicate Workflow IDs
even after completion; an intentional restart requires a new recorded epoch.

Legacy `RunInput.Spec` stays supported for historical and ordinary root inputs.
A generated input has a definition reference instead of an inline spec; reject
ambiguous inputs containing both. A load activity verifies the immutable manifest
and returns the bounded, normalized execution spec and pins. Its result is recorded
in history for replay, so this is **not** a claim that a reference alone removes
definition bytes from history. Bound the serialized resolved input before acceptance
below the deployment's Temporal payload limit, including encoding overhead; the
existing 1 MiB proposal and 128-state bounds alone are insufficient. Reject
oversize graphs explicitly. Definition interpreter changes must be replay compatible
or routed to compatible workers.

Workspace archives, transcripts, diffs, publication receipts, and full output
collections stay in authenticated blob/artifact storage. No snapshot archive,
host path, credential, or expiring signed URL enters workflow input, activity
results, signals, or queries. References bind digest, size, tenant, integrity,
and retention owner; activities obtain fresh scoped access. Missing or corrupt
accepted content is a custody error, never a reason to regenerate it.

## 4. Admission and atomic scheduling

Expose the same agent tools and signed attempt authority as the runner.
The tool front end durably records a bounded request and wakes the parent via an
authenticated, occurrence-bound update or signal. An acknowledgement of delivery
is not child acceptance. The tool awaits the durable admission receipt; transport
timeouts may return pending and reconnect with the same idempotency key.
Agent-supplied invocation outputs are not authority to yield or start a child.

The engine workflow runs admission as an activity:

1. Commit the parent's stage-start event before dispatch. Resolve and verify the
   actual committed occurrence and attempt via an idempotent activity; carry that
   `ChildWorkflowOrigin` into the invocation envelope. Require live journaling and
   a durable shared journal/authority backend for this capability.
2. Validate the proposal with the existing closed schema, compiler, preview gate,
   allowlists, non-recursion rule, publication ceiling, and repository policy.
   Generated definitions stay outside the scheduled workflow catalog. Require the
   current manual-trigger shape. Phase 1 additionally rejects all parallel/map
   states before acceptance.
3. Reserve a pending handoff against the exact active attempt; stop/join the
   parent's writers and durably retain its checkpoint and snapshot. Pending
   requests are distinguishable from accepted child receipts. Recheck the attempt
   fence after quiescence, including cancellation/revocation races.
4. In the existing admission transaction, reserve sequence, one-child occurrence
   slot, family budget and retention capacity, frozen manifest/snapshot ownership,
   lineage, and a start intent explicitly owned by the Temporal parent. Commit
   acceptance only when all prerequisites are durable.
5. Return the immutable receipt to the workflow. After admission, revalidate live
   revocations/readiness in activities before effects. Changed policy can block
   execution but cannot rewrite accepted source or erase custody.

Use the existing shared admission rules and transaction semantics. Process-local
locks or an instance-local queue database are insufficient for the HA contract:
the deployed authority backend must provide cross-worker uniqueness, CAS fencing,
and durable blob ownership. Backend qualification is an activation prerequisite,
not an assumption supplied by Temporal.

There is no transaction spanning the admission database and Temporal. Bridge
them with the retained start intent and idempotent activity result: a crash after
acceptance re-reads the same receipt; replay then emits the same child-start
command. Runner launchers must ignore engine-owned intents. A reconciler wakes
the owning parent or settles a cancelled/orphaned intent; it never starts an
independent root execution as a substitute.

Likewise, before any effectful stage dispatch, durably reserve its attempt and
execution intent. Activity retries reconcile that reservation and any exact pod
custody before dispatching. Temporal command durability is not exactly-once
external execution; journal, database, and pod/provider effects still need their
existing idempotency and uncertain-effect handling.

## 5. Child execution and return

The parent calls `workflow.ExecuteChildWorkflow` with the registered workflow type
for **`engine.Run`**, passing the generated input and lineage. No activity calls
`client.ExecuteWorkflow` to create an unrelated root. Wait for
`GetChildWorkflowExecution().Get` to confirm start before marking the retained
intent started. Record both the stable Workflow ID and observed Temporal Run ID.
Then wait on the child future without occupying a stage activity or pod.

Set these child options explicitly:

- `ParentClosePolicy: PARENT_CLOSE_POLICY_REQUEST_CANCEL`.
- `WaitForCancellation: true`.
- A pinned execution deadline consistent with the family wait policy.
- Workflow retry maximum of one; stage retries retain the existing engine policy.
  Re-executing an entire graph automatically could repeat provider effects.

The [Temporal Go child-workflow contract](https://docs.temporal.io/develop/go/workflows/child-workflows)
defines child futures and parent-close behavior. Request-cancel allows the child
to perform cleanup; it does not prove that cleanup finished. During normal family
cancellation, fence new admissions first, explicitly cancel the child context,
and await settlement using a bounded disconnected cleanup context. Parent closure
also requests cancellation as a fallback. Do not use abandon or treat immediate
termination as successful workspace settlement.

The child runs ordinary engine stage admission and placement with its narrowed
grant. Stop/join every writer before capturing the result. A finalization activity
stores result custody and returns a bounded result envelope containing
`childworkflow.TerminalResultInput` semantics plus child identity, result receipt
digest, and optional workspace reference. The current type bounds UTF-8 summary
to **8 KiB**, references to 16, and each reference to 1,024 bytes. The summary
bound is not an 8 KiB bound on the entire envelope.

`RunResult.Outputs` currently contains per-stage envelopes. Generated children
need an additive, versioned terminal-result-reference field and must omit bulk
`Outputs` on their new return path. Persist those outputs as artifacts instead.
Ordinary and historical root result encoding stays unchanged. Before resuming,
an activity verifies the receipt, lineage, result digest, and stopped-writer
custody. The parent receives the bounded result as an untrusted-data artifact
in the continuation of the **same stage occurrence**, with the appropriate new
attempt identity. A child error or timeout without a normal return goes through
the same trusted finalizer; it cannot fabricate a successful result.

Returned workspace references do not apply changes. Preserve the existing
merge/replace/discard protocol, exact expected parent revision, stopped-writer
checks, and retained publication outcomes. A disposition conflict keeps the parent
parked. The occurrence child slot remains occupied until verified disposition/
acknowledgement; this slot differs from the scheduler concurrency permit.

## 6. Waiting, deadlines, and capacity

The durable sequence is: accepted → parent parked → child started/running →
terminal custody retained → disposition settled → parent capacity reacquired →
parent continued. Each transition is idempotent and journaled. A parent worker,
pod, MCP connection, or daemon may disappear between any two transitions.

At the parked boundary, complete the dispatch activity after verified writer
stop and checkpoint custody. Release its pod, worker activity slot, and the
parent's scheduler concurrency permit through an idempotent capacity activity.
Retain run identity, budget accounting, reservation ownership, and workspace
custody. The child uses normal fair admission and tenant quotas. Even a tenant
with a single execution slot can therefore run a child. Parent resumption must
fairly reacquire capacity before starting writers; replay cannot mint a permit.
Fairness is first-come by waiting parent (#8174). A parent whose resume is
refused for capacity holds a claim on the next freed slot. The claim beats new
starts and child admissions, and it lapses after `ContinuationClaimTTL` without
a retry. Terminal-result processing must not require that execution slot.

Keep three clocks distinct:

| Clock | Contract |
| --- | --- |
| Active stage time | Charge only active execution segments; persist consumed duration/usage across handoff. Child waits neither consume an agent timeout nor reset spent budget. |
| Durable child wait | Pin an absolute wait deadline at parking, or explicitly pin no deadline where policy permits. Use workflow timers and `workflow.Now`, never worker wall time. |
| Transport and cleanup | Bound tool polls and cleanup/reconciliation attempts independently; reconnects and worker restarts never extend the durable deadline. |

The inherited root/family deadline remains an upper bound. On wait expiry, record
the reason, fence continuation, and request child cancellation. Pending cleanup
stays observable and retryable; never claim confirmed cancellation merely because
the child future closed or a cleanup timeout fired. If the parent is forcibly
terminated, a durable family reconciler uses retained authority and exact custody
to finish cleanup. Storage pins survive until family settlement and retention
requirements are met. Parent/child completion races retain the first verified
terminal result and never resume a cancelled parent.

## 7. Replay, upgrades, and tenant isolation

Gate the new command path with
`workflow.GetVersion(ctx, "engine-child-workflows-v1", workflow.DefaultVersion, 1)`
at the earliest point that changes refusal/command behavior. The default branch
preserves historical refusals and command order. The version-one branch requires
an explicit pinned child protocol capability and preview policy; the legacy
nil-preview permissive fallback does not authorize children. Runs without child
capability follow their existing path without added child commands.

Use separate change IDs for later fan-out protocols. Tests must cover histories
with no marker, with the new marker, and across every wait boundary. Retain old
branches while their histories can replay. [Temporal's versioning guidance](https://docs.temporal.io/develop/go/workflows/versioning)
explains the marker mechanism; it does not make an old worker understand new code.
Deploy capable workers first and route opted-in executions to a compatible
versioned worker deployment/task queue. Rollback disables new admissions and
keeps compatible workers for existing families. Do not send new child histories
to workers that only implement the old refusal.

Bound family invocation counts, graph steps, and retained history growth.
Phase 1 does not Continue-As-New a parent with unfinished children: the old
parent run's close policy would interfere with custody. Refuse further child
admission before exhausting the history budget, retain existing children, and
settle the family. A future continuation protocol must explicitly transfer
ownership and prove cancellation/recovery across the run boundary.

All mutable I/O, credential/policy checks, blob access, journal identity lookups,
and current capacity decisions occur in activities. Workflow code only uses
pinned inputs, recorded activity results, deterministic timers, and ordered data.
No live registry lookup, network fetch, map iteration determining commands, or
random identifier generation occurs during replay.

Every request and reference is tenant/gaggle scoped and verified against the
authenticated parent, not agent-provided IDs. Temporal task queues and searchable
lineage are routing/observability, not authorization. Namespace/service ACLs,
artifact access, quotas, and retention enforce isolation. Journal events expose
admission, start, wait, cancellation requested/confirmed, result, disposition, and
continuation with stable correlation IDs and bounded redacted reasons. Portal
lineage must distinguish business child identity from execution epochs.

## 8. Phase 2: conditional fan-out

Land static parallel execution before selection/maps. Share the runner's pure
source-validation, roster-freezing, integrity, identity, and join reducers;
implement scheduling with Temporal primitives. Parallel branches are branches
of the engine walk, not newly admitted generated-child identities.

For selective parallels, freeze the selected branch names and reason/source
digest once before scheduling. Record every unselected optional branch as
`skipped`; preserve required branches and declaration order. For maps, validate
and freeze the canonical roster once, including stable item identity, declared
order, verified digest, and item input provenance. Read source artifacts through
an activity; record its bounded result and roster reference in history. Resume
from frozen data even if the planner's latest artifact changed.

Bound both roster cardinality and active dispatches. Schedule in deterministic
order; collect settlements by stable identity, not completion order. An empty map
enters its join once. Fail-fast cancels running work and records every unstarted
item as cancelled. Retry/timeout applies to an item or branch occurrence; a
completed sibling is not rerun. Preserve isolated workspace ownership, qualified
outputs, integrity floors, and exact join completeness checks.

Once parallel parents can request children, a waiting branch releases its own
permit. Release a run-level permit only when all runnable branches are parked
and reacquire it before any branch resumes. Keep one unfinished generated child
per branch occurrence, not one per whole parent workflow. Retain the fan-out
design's bounds and nesting restrictions. No expression language or mutable
graph generation is introduced by conditional selection.

## 9. Delivery and qualification

Each implementation PR must carry its production caller and failure tests.
Refusals remain until the complete applicable path is wired.

| Slice | Deliverable and activation boundary |
| --- | --- |
| ECW-1 | Shared versioned wire contracts, frozen manifest, committed origin activity, replay seam; engine child execution remains refused. |
| ECW-2 | HA admission/start-intent fencing, parent handoff, slot release, `ExecuteChildWorkflow(engine.Run)`, terminal custody and disposition continuation for serial graphs. |
| ECW-3 | Family cancellation/reconciliation, deadlines, worker routing, shared parity and real-backend qualification; only then lift child refusal behind the existing preview gate. |
| EFO-1 | Static parallel engine branch scheduling, joins, custody, and shared runner parity; lift static-parallel refusal only. |
| EFO-2 | Selection/maps, frozen-source recovery, bounds, cancellation, generated-child composition; lift each refusal with its conformance tests. |

Avoid an import cycle: `internal/childworkflow` already imports `internal/engine`
through proposal validation. Put neutral DTOs/validation in an appropriate shared
package and inject activity-side admission/custody interfaces. Do not import the
whole childworkflow service into engine workflow code.

Run targeted packages with `go test -race -timeout 30m`; run `make build` for
implementation changes. Phase 1 acceptance requires:

1. Shared runner/engine fixtures for serial task/gate children, failure, repeated
   sequential children, repasses, cancellation, result dispositions, and capacity.
   Remove the blanket `ChildWorkflowOrigin` parity exclusion only when fixtures
   verify committed event binding; compare identities by their journal relation,
   not identical backend sequence numbers.
2. Temporal test-suite cases with actual registered parent/child workflows,
   activity retry/ack loss, start failure, timeout/completion races, duplicate
   requests, stale attempts, forged tenant/origin, and preview refusals. Assert
   exactly one accepted child and the expected continuation.
3. Saved history replay with `worker.WorkflowReplayer`: old histories and new
   histories paused after admission, child start, parking, result capture, and
   continuation. `OnGetVersion` unit mocks supplement real replay, not replace it.
4. Fault injection between every database commit, activity acknowledgement, child
   command, capacity transition, and journal emission. Corrupt/missing blobs and
   conflicting retries fail closed without creating a second execution.
5. Capacity-one tests proving the parent releases its slot and reacquires before
   continuation; deadlines survive worker replacement and do not reset usage.
   Verify cancellation while queued, running, finalizing, and awaiting disposition.
6. An explicitly provisioned disposable Temporal/Kubernetes qualification with
   two service replicas and the durable shared authority backend: rolling upgrade,
   worker loss, parent termination, real parent-close cancellation, pod writer
   settlement, and cross-tenant denial. In-memory tests alone do not establish HA.

Phase 2 adds all applicable conformance cases in the fan-out design §14 to the
shared harness, including changed-source resume, randomized completion order,
bounded concurrency, skipped/unstarted members, empty maps, and exact joins.
Neither this design PR nor foundational tests alone close #155 or #1310.
