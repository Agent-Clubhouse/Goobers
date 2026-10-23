# Design: Child-workflow authority for engine parents on the cloud

> Status: draft — for adversarial review before any code (#8085 step b)
> Area: cloud control plane, engine worker, child-workflow grants, threat model
> Related: #8085, #7758, #7818, docs/design/engine-child-workflows.md, docs/design/control-plane-ha.md

## 1. Problem

On the cloud, an engine parent's agentic stage cannot request a generated child.
The engine side of admission exists: the control role freezes and binds children
for engine workers (#7879). What is missing is the **request side**. A parent
stage needs a short-lived child-workflow grant for its exact attempt, and the
control plane must accept children only while that attempt is the active one.

Locally, this is the daemon's contained-parent exchange
(`daemonCredentialService.parentAttempt`, `parentAccessPlane`). A parent pod
holds a signed workflow-parent identity bound to a retained parent *contract*.
It exchanges that identity at the parent-access route. The daemon reads the
parent's journal, checks that the attempt is active and its writer is unjoined,
and runs `childworkflow.GrantIssuer`. That machinery is about 5.7k lines of
`cmd/goobers` built around contained parent pods. The HA design retires it for
the engine: "parents on the engine, removing `containedParentSelection`"
(control-plane-ha.md, the HA-P4-08/09/10 rows).

This note specifies the engine-native replacement. It is a design for review;
nothing here is implemented.

## 2. Decision

**The control plane issues and binds child authority. The engine worker only
requests it, as a trusted host. Every check that matters is made against state
control already holds:**
- the PostgreSQL journal (the stage origin receipt and the active attempt);
- the start-fence row (the parent's pins, as resolved by control at run start);
- the lease-fenced child custody.

The flow:

1. **Origin.** When the engine starts a child-capable stage, it appends
   `StageStarted` through the journal plane (`AppendChildStageStarted`). The
   control-side journal writer returns the committed `ChildOriginReceipt`
   (stage occurrence and attempt ID). This already exists in the engine
   (`runJournal.childAttemptEnvelope`). On the cloud it needs the journal plane
   composed in control (#7818).
2. **Acquire.** At stage dispatch, before the stage pod starts, the worker calls
   a new control route, `POST /api/v1/engine/children/authority`. It
   authenticates with the dedicated worker child-admission credential (the same
   issuer family as #7879's plane) and sends only the run ID and the committed
   origin.
3. **Verify and bind (control, under the controller lease).** Control:
   - loads the run's `control.run_pin` row; a missing row, or an empty required
     pin, refuses;
   - builds the parent identity from those pins and control state, never from
     the journal (§4);
   - opens the parent journal through `journalpg`, verifies the attempt is
     active (§5), and requires the journal identity to agree with the pins;
   - checks the bound Temporal execution: `RUNNING`, with a memo input digest
     equal to the pin (§5);
   - computes `Authority` with `childworkflow.PinnedAuthority` over the
     object-store `GenerationSource`;
   - mints the child-workflow grant with its credential-grant key;
   - binds it in cloud custody with `BindChildAuthority` (CAS on the prior
     grant).

   It returns `{endpoint, bearer}`.
4. **Deliver.** The worker stamps the bearer into the stage pod as a new
   dispatcher-privileged variable, listed in `DispatcherPrivilegedEnv` like
   `GOOBERS_POD_TOKEN`, so `__dispatch-exec` strips it from the stage
   environment. The in-pod harness launch reads it through the existing
   `harness.ChildWorkflowAccessProvider` seam. As locally, it places the bearer
   only in the child-workflow MCP tool's config (`mcpio.ChildWorkflowAccess`).
   The grant is meant for the stage's agent, scoped to its own occurrence. It
   never enters an execution kit, invocation JSON or the journal. Its pod-spec
   exposure equals that of today's pod token. The local contained-parent design
   avoided pod-spec exposure by fetching at runtime; see §9.
5. **Use.** The pod calls the existing child-workflow routes
   (`ChildWorkflowService`) on control. These run `SubmissionService` over cloud
   custody (`ChildQueue`, #8086). Every call re-verifies the grant signature,
   the bound row (`CheckChildAuthority`) and the active attempt
   (`AuthorityResolver.Resolve`, §5).
6. **Release.** When the stage activity ends (success, failure, cancellation or
   heartbeat timeout), the worker calls `DELETE` on the same route. Control
   revokes the binding. If the worker dies first, §5 makes the grant inert.

There is no contained-parent pod, no retained parent contract, and no
workflow-parent pod identity on this path.

## 3. Threat model

**The engine worker is a trusted host**, in the same class as the local daemon.
It is also the precedent HA-P4-13 already relies on: the trusted supervisor
enforces remote token and cost caps. The worker holds the instance's shared
signing-key file (`api.podTokenKeyFile`). From it, it can derive every
domain-separated key: pod tokens, worker bearers, and the credential-grant
domain (`podauth.LoadKeyRingFile(file, CredentialGrantKeyDomain)`).

**A compromised worker can:**
- mint any bearer, including a syntactically valid child-workflow grant;
- write journal events for runs it executes, because it is the journal writer
  for them through the plane. So it can make a stage of such a run appear
  active;
- therefore request child authority, and then children, for any parent stage of
  **this instance** whose run exists in the start-fence table. This is bounded
  below.

**A compromised worker cannot:**
- **Act on another instance.** Every route, row and generation is
  instance-scoped, and the bind checks the journal and start-row instance.
- **Choose the policy.** Capabilities, goobers, PR publication and the child
  ceiling come from the parent stage's `childWorkflows` policy in the **pinned
  generation**, recompiled by control from the object-store archive. The
  generation and digests come from `control.run_pin`, which control wrote (§4).
  The worker cannot widen them.
- **Redirect where children act.** The repository children work on and publish
  to is `run_pin.repo_ref`, the repository the start fence started the run on.
  A journalled `WorkspaceRepository` that disagrees refuses (§4).
- **Get a child accepted without a control-side binding.** A self-minted grant
  fails `CheckChildAuthority`, because only control writes the bound row, under
  its lease.
- **Revive a finished or superseded attempt.** `Resolve` re-reads the journal
  on every use. A compromised worker can still lie *forward*, by never writing
  `StageFinished`. That is bounded by the grant TTL and by the Temporal
  liveness check (§5).
- **Read or alter other runs' custody**, or bypass `SubmissionService`'s proposal
  validation.

This is the same blast radius as a compromised local daemon, which also holds
the key file and writes the journal. It is strictly smaller than the daemon's,
because control, not the worker, validates proposals and holds custody.

**Residual risk (follow-up, not this design):** the worker can derive the
credential-grant key from the shared key file. A control-only key file for the
credential-grant domain would remove "mint a grant" from the list above. The
bound-row check already makes such a grant useless, so this is
defence-in-depth.

## 4. Pins: do not trust worker-asserted values

The worker sends only `run` and the origin. **The parent identity passed to
`PinnedAuthority` is built entirely from control-held state. The worker-written
journal identity is never an input to authority.** It is only cross-checked, and
any disagreement refuses.

| Field of the parent identity | Source | Why |
|---|---|---|
| `InstanceID` | the control store's own instance | the bind is instance-scoped by construction |
| `RunID`, `Gaggle`, `Workflow`, `WorkflowVersion` | `control.run_pin` | written by the start fence (below) |
| `ConfigGeneration`, `WorkflowDigest`, `GooberDigest` | `control.run_pin` | as above. `PinnedAuthority` recompiles the parent stage from the pinned generation archive and requires both digests to match |
| `WorkspaceRepository` | `control.run_pin.repo_ref` (the `RunInput.RepoRef` the run was started with) | the repository children act on and publish to is control-authored |
| `Driver` | constant `engine`: control sets it, it isn't pinned | the cloud starts engine runs only |
| `Child` | constant nil: control sets it | a `run_pin` row exists only for a top-level run the start fence started. A missing row refuses, which keeps the I2 recursion guard |
| Stage occurrence, attempt ID, attempt number, branch | committed `ChildOriginReceipt` in the journal | written by the control-side journal writer, not asserted by the worker |

**The `run_pin` row is required.** If it's missing (for example, a run started
before migration 0022), or a field the bind needs is empty (`config_generation`,
`workflow_digest`, `goober_digest`, `repo_ref`), the bind refuses
(`ErrAuthorityUnavailable`). The journal is read for **two** things only:
- the active attempt (§5);
- a cross-check that its identity's `InstanceID`, `RunID`, `Gaggle`, `Workflow`,
  `WorkflowVersion`, `WorkflowDigest`, `GooberDigest`, `ConfigGeneration`,
  `Driver = engine`, `Child = nil` and `WorkspaceRepository` (nil, or equal to
  the pin) equal the values above.

Any disagreement refuses. A worker that journals a different repository, digest
or generation gets nothing.

**Gap closed by migration 0022 (`control.run_pin`).** The start-fence row
(`control.acceptance`) holds only the trigger payload, `state` and `run_id`, and
the journal identity is the worker's `CreateRun`. Control resolves the run's
`engine.RunInput` immediately before `starter.Start`
(`control.Runtime.drain` → `StartInputResolver`). The start fence now records
its pins in `control.run_pin`, a sibling table keyed `(instance_id, run_id)` with
a cascade from the acceptance row, inside the **same fenced transaction** that
confirms or adopts the Temporal start (`ConfirmStart`/`AdoptStart`). The row
holds exactly:
- `acceptance_id`, `gaggle`, `workflow`, `workflow_version`;
- `workflow_digest`, `goober_digest`, `config_generation`;
- `repo_ref` (JSON), `branch_namespace`;
- `input_digest` (the start-owner memo digest), `owner_epoch`.

`driver` is not stored, because it is the constant above. The classified start
runs exactly that `RunInput`: an existing workflow is adopted only when its
start-owner memo carries the same input digest. So a retry or a successor adopt
writes identical pins, and different pins for a recorded run refuse
(`ErrRunPinsDiffer`).

**Pins are evidence, never a start gate.** Refusing to confirm a Temporal start
because a pin is empty would wedge legitimate runs; a prototype did exactly that
in the control failover tests. So the start fence records whatever the started
input carries and requires only the run and acceptance identity. The bind
refuses its own operation when a pin it needs is empty. #8085 (b) carries a test
per required pin.

This is much smaller than #7818: one migration, a writepaths row and a
start-fence change, already built on a branch. It does **not** remove the #7818
dependency. The origin receipt in step 1 is itself a journal-plane write, so no
engine parent can start a child-capable stage until the journal plane is
composed.

## 5. Attempt currency, revocation and replacement

**Active attempt.** Control runs `childworkflow.VerifyActiveStage`, unchanged,
against the `journalpg` reader. It currently takes `*journal.Reader`; it uses
only `Identity()` and `Events()`, so it widens to `journal.RunReader`. It
requires:
- the run to be in the running phase;
- the origin's `StageStarted` to exist;
- no later event to supersede it (`supersedesStage`, enclosing parallel
  finished);
- the started event's `Seq`, `Attempt` and `Branch` to equal the receipt.

**Bound execution, independent of the worker's journal.** The bind and each
`Resolve` call `DescribeWorkflowExecution(workflowID = run_id, runID = "")`
(control holds the Temporal client; an empty run ID describes the latest run of
that workflow ID). Two checks must hold:
- the execution's status is `RUNNING`;
- its start-owner memo (`StartOwnerMemoKey`) decodes and its `InputDigest`
  equals `control.run_pin.input_digest`.

The memo is written by control's own start fence, so this binds the live
execution to the exact pinned start. A worker that stops writing, or journals
past its parent's end, cannot keep a grant alive. The pending-activity list is
not used, because activity IDs are not a stable contract.

- **Reset.** A Temporal reset of the parent creates a new run under the same
  workflow ID from the original start event, so its memo, and therefore
  `InputDigest`, is preserved. The latest run must be `RUNNING` with the same
  digest. Grants then continue to be governed by the active-attempt check:
  attempts the reset discarded are superseded in the journal and refuse.
- **Continue-as-new.** The engine `Run` never continues-as-new today. If it
  ever does, the new run must carry the same start-owner memo. Otherwise the
  digest check fails closed and the parent's grants refuse until re-bound.

**Fencing and epoch adoption.** Bind, revoke and every custody write run under
`withFence`. A replica that lost the lease cannot bind; the plane answers 503,
and the worker retries (#8074 classes). A controller that takes over the lease
first adopts **every** earlier epoch's child custody in one fenced, idempotent
transaction, before the first call of its epoch is served
(`acceptance.AdoptStaleChildCustody`, run once per epoch by
`childengine.Fenced`; implemented in #8095). The previous exact-predecessor
`AdoptChildren` is removed. A stale epoch that is somehow still observed returns
the distinct, retryable `acceptance.ErrStaleChildEpoch`, never
`ErrParentCancelled`, which is a permanent refusal.

**Mapping against `GrantIssuer`, one to one:**

| `GrantIssuer` behaviour | Engine-native | Why |
|---|---|---|
| `PrepareStage` → authority from the active journal attempt and the pinned generation | **kept**: the same `PinnedAuthority` and `VerifyActiveStage`, journal via `journalpg` | the same invariant, a different backend |
| refuses a prepared authority that already carries a `GrantID` | **kept** | prevents re-binding a delivered grant |
| `previousGrant`: the same attempt may not steal a live or revoked grant; a new attempt replaces by CAS | **kept**: `BindChildAuthority(binding, previous, now)` on cloud custody | attempt replacement stays CAS-ordered across replicas |
| a still-active attempt may renew after expiry; a revoked one may not | **kept**: the worker re-calls the route on renewal | long waits stay bounded by TTL |
| re-run `PrepareStage` after binding; on drift, revoke and fail (`samePreparedAuthority`) | **kept**: recheck after bind, under the same fence | closes the bind/drift race |
| bearer registered with the journal secret scrubber (`Secrets.RegisterUntil`) | **changed**: control registers it with its own scrubber; the worker registers the value it receives with the stage's scrubber | the bearer must never land in either side's journal |
| revoker: an older attempt finishing must not revoke its replacement (`ErrChildAuthorityChanged` ignored) | **kept**: `DELETE` revokes only the exact bound grant ID. `ErrChildAuthorityChanged` (already replaced) is success. `ErrStaleChildEpoch` and unavailability are retried after adoption, never reported as success | the same replacement safety, plus takeover |
| in-process provider called by the harness launcher | **changed**: an HTTP route called by the worker at dispatch | the engine worker is not the API host |
| `Endpoint` = the daemon API | **changed**: the control API endpoint | the pod talks to control |

**Disposition revisions on attempt replacement (a gate for #8085 (c)).** A
retried parent attempt gets a new `AttemptID`, and re-requests its child's
disposition with `expected` = the current request digest. The SQLite store
handles this as a revision: it archives the prior request in the disposition
history and rebinds it to the new attempt. It returns
`ErrChildDispositionReconcile` or `ErrChildDispositionHistoryFull` at its
limits.

The cloud store's `RequestChildDisposition` has no supersede or revision path,
so the retried attempt gets `ErrConflict` forever. It never returns the
reconcile or history-full sentinels, and its `ChildDispositionHistory` is a
stub. Before (c) wires cloud custody behind `ChildQueue`, cloud custody must
either implement the same revision semantics or refuse revisions with a
**distinct** sentinel that the request path surfaces as an attempt-replacement
refusal. Either way, one shared `ChildQueue` disposition contract test runs
against both stores. Attempt replacement is otherwise not equivalent across
backends.

## 6. Grant scope, TTL and keys

- **Scope.** Unchanged `podauth.ChildWorkflowGrant`: `{Gaggle, RunID,
  StageOccurrence, AttemptID, ConfigDigest, PolicyDigest}`. It is valid only on
  the child-workflow routes, and only for that run and occurrence. `ConfigDigest`
  and `PolicyDigest` in the grant come from control's `PinnedAuthority`, never
  from the worker.
- **TTL.** `min(podauth.MaxChildWorkflowGrantTTL = 24h, stage MaxDuration +
  heartbeat timeout)`. The stage activity's budget bounds it. Longer waits use
  the same-attempt renewal row above. After expiry, an unrevoked grant from a
  dead worker is refused by the expiry in `CheckChildAuthority`, and earlier by
  the Temporal liveness check.
- **Keys.**
  - The worker authenticates the acquire and release route with the
    **child-admission worker bearer** (`goobers-worker-child-admission.`, its
    own MAC domain; #7879). It is never a pod or plane bearer.
  - Control mints the child-workflow grant in the **credential-grant key
    domain**. The grant MAC domain stays separate from pod tokens, worker
    bearers and child-pod and workflow-parent pod tokens.
  - The stage pod's own run token is not used for child authority.

## 7. Local invariants (`parentattemptauthority.go` and `parentaccessowner.go`)

| Local invariant | Engine-native | Justification |
|---|---|---|
| I1. The principal is the workflow-parent pod issuer with a valid contract digest and `run:<id>` subject | **replaced**: the principal is the worker child-admission issuer; the run ID is in the path | no contained parent pod exists. The authenticating party is the trusted host, as with the daemon's own in-process issuer |
| I2. The parent run exists, the identity's run ID matches, and the parent is not itself a child (`id.Child == nil`) | **kept**: a `control.run_pin` row is required (one exists only for a top-level run the start fence started); `Child` is constant nil; the journal identity must agree | the same recursion and identity guard, from control-authored state |
| I3. The retained parent contract decodes, carries `ParentOrigin`, and its identity deep-equals the journal identity | **replaced**: the origin comes from the committed `ChildOriginReceipt`; the parent identity is built from `run_pin` and control state, and the journal identity must deep-agree with it | the contract existed only to bind a pod to its attempt. With no pod-held identity, the control-written receipt and pin row are the binding |
| I4. `active()`: `VerifyActiveStage`, plus the started event's seq, branch, attempt and start time equal the contract | **kept**, with the receipt in place of the contract: seq, branch and attempt from the receipt | the start-time check is dropped, because the receipt seq already identifies the exact event |
| I5. `custody()`: the physical writer is not yet joined (`ParentCustodyPending`) | **replaced**: the bound-execution check (`RUNNING`, plus memo `InputDigest` = `run_pin.input_digest`, §5), plus the activity-end release | it guarded a contained pod's late output. Engine stage output flows through the worker's activity, whose end releases the grant |
| The envelope is built from the pinned machine; the task must be agentic with `childWorkflows` | **kept**: `PinnedAuthority`/`StageCatalog` refuse otherwise | the same stage gate |
| After issuing, recheck active and custody, else revoke | **kept**: recheck active and liveness after bind, else revoke | the same race closure |

## 8. Dependencies and delivery

1. **#7818.** The journal plane is composed in control. This is required for the
   origin receipt (step 1), and for `journalpg` reads.
2. **Start-fence pins** (§4). Migration 0022 `control.run_pin`, written in the
   start fence's confirm/adopt transaction. Built on a branch; its PR opens when
   this note is approved, and can land before #7818.
3. **#8085 (b).** The control authority route (bind, revoke, renew), worker
   dispatch acquire and release, delivery as a dispatcher-privileged env var
   (§2 step 4), the harness provider, `VerifyActiveStage` widened to
   `RunReader`, and a test per required empty pin.
4. **Disposition parity (gate before (c)).** Cloud `RequestChildDisposition`
   either implements supersede/revision (archive, rebind, reconcile and
   history-full sentinels, real `ChildDispositionHistory`) or refuses revisions
   with a distinct sentinel. A single `ChildQueue` disposition contract test
   runs against both the SQLite and the cloud store (§5).
5. **#8085 (c).** `ChildWorkflowService` on control over `ChildQueue`, with
   `Resolve` = §5 checks, and per-child generation pins.
6. **#8085 (d).** End-to-end, including:
   - a worker kill mid-stage (the grant goes inert at parent completion or TTL);
   - a control failover between bind and submit, where the successor adopts
     every earlier epoch's custody and then serves the submit;
   - a Temporal reset of the parent.

Production keeps refusing `task.childWorkflows` until (d); #7758 follows.

## 9. Open questions for review

- Should the Temporal liveness check also confirm a pending activity for the
  stage? It is deliberately not required today (§5).
- **Resolved: delivery (Q2).** The grant is delivered as a dispatcher-privileged
  env var (§2 step 4), stripped from the stage environment and placed only in
  the child-workflow MCP tool config. Its pod-spec exposure equals today's
  `GOOBERS_POD_TOKEN`. That's consistent, simple, and requires no new pod
  identity. Fetching at runtime (an attempt-bound pod token over the receipt
  digest, like `MintChildPod`) is a hardening follow-up for all pod bearers
  together, not specific to this grant. No Kubernetes Secret object is used.
- **Resolved: pin placement.** A sibling `control.run_pin` table, so the
  acceptance row's hot path stays narrow (§4).
- Should the control-only credential-grant key file (§3 residual) be a
  prerequisite rather than a follow-up?
