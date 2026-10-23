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
   - opens the parent journal through `journalpg`;
   - verifies the attempt is active (§5);
   - checks that the journal identity equals the start-fence row's pins (§4);
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
  generation**, recompiled by control from the object-store archive. The pins
  are checked against the start-fence row, which control wrote (§4). The worker
  cannot widen them.
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

The worker sends only `run` and the origin. Control obtains every pin itself:

| Pin | Source today | Trusted because |
|---|---|---|
| `InstanceID`, `RunID`, `Gaggle`, `Workflow`, `WorkflowVersion` | journal identity (`journalpg` `RunReader.Identity`) | **cross-checked** against the start-fence row |
| `ConfigGeneration`, `WorkflowDigest`, `GooberDigest`, workspace repository | journal identity | **cross-checked** against the start-fence row (see gap below) |
| Stage occurrence, attempt ID, attempt number, branch | committed `ChildOriginReceipt` in the journal | written by the control-side journal writer, not asserted by the worker |
| Parent stage policy, capabilities, goobers | `PinnedAuthority` recompiled from the pinned generation archive | control-side compile; the workflow and goober digests must match |

**Gap.** The start-fence row (`control.acceptance`) holds the trigger payload,
`state` and `run_id`. **It does not hold the run's pins.** The journal identity
is created by the worker's `CreateRun` through the journal plane. So without a
control-authored copy, the pins in the table above are worker-originated.

**Fix (smaller than #7818, and independent of it).** Control resolves the run's
`engine.RunInput` (`control.Runtime.drain` → `StartInputResolver`) immediately
before `starter.Start`. The start fence's fenced `accepted → dispatching`
transition would persist the resolved pins next to the acceptance row before the
Temporal workflow starts. That is one expand migration and one store method:
- `config_generation`
- `workflow_digest`
- `goober_digest`
- `workflow`
- `workflow_version`
- `repo_ref` (JSON)
- `driver`

The bind then requires the journal identity to equal those pins exactly. This is
**much smaller than #7818**: a migration, a writepaths row and a start-fence
change, against composing the whole journal plane. It does **not remove** the
#7818 dependency, though. The origin receipt in step 1 is itself a journal-plane
write, so no engine parent can start a child-capable stage until the journal
plane is composed. Both are needed. The pin row is what turns the journal from
the worker's word into a checked fact.

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

**Liveness independent of the worker's journal.** The bind and each `Resolve`
also check the parent's Temporal workflow with `DescribeWorkflowExecution`
(control holds the Temporal client):
- its status must be `RUNNING`;
- the workflow ID and run ID must equal the run's bound execution.

A worker that stops writing cannot keep a grant alive past its parent workflow's
end. The pending-activity list is not required for correctness and is not used:
activity IDs are not a stable contract.

**Fencing.** Bind and revoke run under `withFence`. A replica that lost the lease
cannot bind; the plane answers 503, and the worker retries (#8074 classes).

**Mapping against `GrantIssuer`, one to one:**

| `GrantIssuer` behaviour | Engine-native | Why |
|---|---|---|
| `PrepareStage` → authority from the active journal attempt and the pinned generation | **kept**: the same `PinnedAuthority` and `VerifyActiveStage`, journal via `journalpg` | the same invariant, a different backend |
| refuses a prepared authority that already carries a `GrantID` | **kept** | prevents re-binding a delivered grant |
| `previousGrant`: the same attempt may not steal a live or revoked grant; a new attempt replaces by CAS | **kept**: `BindChildAuthority(binding, previous, now)` on cloud custody | attempt replacement stays CAS-ordered across replicas |
| a still-active attempt may renew after expiry; a revoked one may not | **kept**: the worker re-calls the route on renewal | long waits stay bounded by TTL |
| re-run `PrepareStage` after binding; on drift, revoke and fail (`samePreparedAuthority`) | **kept**: recheck after bind, under the same fence | closes the bind/drift race |
| bearer registered with the journal secret scrubber (`Secrets.RegisterUntil`) | **changed**: control registers it with its own scrubber; the worker registers the value it receives with the stage's scrubber | the bearer must never land in either side's journal |
| revoker: an older attempt finishing must not revoke its replacement (`ErrChildAuthorityChanged` ignored) | **kept**: `DELETE` revokes only the exact bound grant ID | the same replacement safety |
| in-process provider called by the harness launcher | **changed**: an HTTP route called by the worker at dispatch | the engine worker is not the API host |
| `Endpoint` = the daemon API | **changed**: the control API endpoint | the pod talks to control |

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
| I2. The parent run exists, the identity's run ID matches, and the parent is not itself a child (`id.Child == nil`) | **kept**: `journalpg` identity, plus the start-fence row pins | the same recursion and identity guard |
| I3. The retained parent contract decodes, carries `ParentOrigin`, and its identity deep-equals the journal identity | **replaced**: the origin comes from the committed `ChildOriginReceipt`; the pins equal the start-fence row | the contract existed only to bind a pod to its attempt. With no pod-held identity, the control-written receipt and row are the binding |
| I4. `active()`: `VerifyActiveStage`, plus the started event's seq, branch, attempt and start time equal the contract | **kept**, with the receipt in place of the contract: seq, branch and attempt from the receipt | the start-time check is dropped, because the receipt seq already identifies the exact event |
| I5. `custody()`: the physical writer is not yet joined (`ParentCustodyPending`) | **replaced**: Temporal liveness, plus the activity-end release | it guarded a contained pod's late output. Engine stage output flows through the worker's activity, whose end releases the grant |
| The envelope is built from the pinned machine; the task must be agentic with `childWorkflows` | **kept**: `PinnedAuthority`/`StageCatalog` refuse otherwise | the same stage gate |
| After issuing, recheck active and custody, else revoke | **kept**: recheck active and liveness after bind, else revoke | the same race closure |

## 8. Dependencies and delivery

1. **#7818.** The journal plane is composed in control. This is required for the
   origin receipt (step 1), and for `journalpg` reads.
2. **Start-fence pins** (§4). A new expand migration and start-fence write. It
   can land before #7818.
3. **#8085 (b).** The control authority route (bind, revoke, renew), worker
   dispatch acquire and release, Secret delivery, the harness provider, and
   `VerifyActiveStage` widened to `RunReader`.
4. **#8085 (c).** `ChildWorkflowService` on control over `ChildQueue`, with
   `Resolve` = §5 checks, and per-child generation pins.
5. **#8085 (d).** End-to-end, including a worker kill mid-stage (the grant goes
   inert at parent completion or TTL) and a lease takeover between bind and
   submit.

Production keeps refusing `task.childWorkflows` until (d); #7758 follows.

## 9. Open questions for review

- Should the Temporal liveness check also confirm a pending activity for the
  stage? It is deliberately not required today (§5).
- Delivery: should the pod fetch its grant at runtime, as the local
  contained-parent exchange does, to keep it out of the pod spec? That needs an
  attempt-bound pod identity (a pod token over a digest of the receipt, like
  `MintChildPod`). The env-var delivery in §2 step 4 matches today's pod-token
  exposure and is simpler.
- Should the start-fence pins go on `control.acceptance`, or on a sibling
  `control.run_pin` table keyed by `run_id`? Recommendation: a sibling table, so
  the acceptance row's hot path stays narrow.
- Should the control-only credential-grant key file (§3 residual) be a
  prerequisite rather than a follow-up?
