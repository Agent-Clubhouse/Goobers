# Design: Orchestrator pattern

> Status: draft — experimental extension of agent-authored child workflows
> Area: DSL, child admission, MCP, local runner, Temporal, workspace custody
> Verified: 605c88f0c4924585d0727878ec3f9d687a312c9f (2026-10-20)

An opted-in parent agent should plan work, choose bounded helpers, exchange
messages and questions, and review their results. Every helper remains an
ordinary child Workflow with retained DSL and immutable admission evidence.
The host enforces policy; agent instructions describe policy but cannot grant it.
This document specifies v1 work on the experimental orchestration branch. It
makes no claim that these extensions are available on main and does not change
DSL 3.1 preview status.

## Foundation and scope

The existing [child-workflow design](agent-authored-child-workflows.md) supplies
stage-occurrence identity, pinned definitions, bounded child proposals, separate
workspaces, durable waits, lineage, results and merge/replace/discard custody.
The current MCP tools are `validate_child_workflow`, `start_child_workflow`,
`get_child_workflow`, and `resolve_child_workflow`
(`internal/mcpio/childworkflow.go`). Admission is in `internal/childworkflow`,
transactional acceptance in `internal/triggerqueue/children.go`, and local launch
in `cmd/goobers/childlauncher.go`. A child result remains a bounded summary
(at most 8 KiB) plus references; chat is not a replacement result store.

The reference behavior is Clubhouse commit `3309665c`, specifically
`src/main/services/clubhouse-mcp/tools/helper-tools.ts`,
`helper-question-tools.ts`, `src/main/services/orchestration-policy.ts`, and
`src/shared/orchestration-policy.ts`. These are behavioral reference points,
not dependencies of this repository. The translation deliberately adds durable
requests and atomic scheduling where the desktop implementation uses live
process state or in-memory pending requests.

V1 includes shorthand compilation, declared harness/model/base selection,
a durable parent wire, question routing, human control levels, and concurrent
children. Recursive orchestration remains forbidden. Sleep/wake is a stretch
slice with explicit custody requirements, not a prerequisite for those features.

## Primitive mapping

| Clubhouse primitive | Goobers DSL and runtime contract |
| --- | --- |
| Orchestrator permission/persona | Agentic task with `childWorkflows`; selected Goober supplies instructions and capabilities. Omission disables child creation. |
| Task and helper persona | `start_helper.task` becomes the single task's `goal`; `goober` references a pinned existing Goober. No new Goober resource is accepted. |
| Helper CLI and model | Task `agent` selection and parent `allowedAgentProfiles` authorize exact Goober/harness/model tuples, subject to host admission and credential ceilings. |
| Branch/folder at creation | Host-owned isolated workspace; `base` selects parent snapshot or a declared repository ref, resolved and pinned before acceptance. Callers cannot supply a filesystem path or destination branch. |
| `start_helper` | Host generates and retains one-stage Workflow YAML, then uses ordinary validation, acceptance, launch and child lineage. |
| `list_helpers` and status | Bounded occurrence-owned child inventory, including queue, execution, question, result and disposition status; existing get tool remains authoritative. |
| `message_parent` | Child-to-parent durable informational message with receipt and parent continuation delivery. |
| Parent replies and helper questions | `ask_parent` creates a durable question; `reply_child` sends guidance or answers an exact question revision. |
| Hands-off / ask-before-start / ask-everything | `control: autonomous / review / approve-each`; human approvals bind immutable operations. |
| Helper access ceiling | Intersection of parent execution grants, DSL delegation policy, selected Goober grants, host credential policy and current revocations. |
| Helper and global limits | Occurrence lifetime and concurrent limits plus tenant/run/host admission quotas and runnable resource limits. |
| Parent waits for helper | Durable continuation releasing execution capacity; messages and results can wake the parent without completing the child. |
| Review and hand-off | Parent requests existing merge/replace/discard with exact result and parent-state preconditions; hand-off is recorded only after custody settles. |
| Dispose | Host cleanup after writer-stop and disposition evidence; never delete the only retained result or equate process exit with acceptance. |
| Sleep and wake | Optional durable suspended child with retained workspace custody, fenced writer and a new execution attempt on wake. |

## DSL contract

The following is a **proposed fragment**, not a currently executable example:

```yaml
childWorkflows:
  allowedGoobers: [implementer, reviewer]
  allowedCapabilities: [agent:model, repo:read]
  maxChildren: 4
  maxConcurrentChildren: 2
  questions: parent-then-human
  control: review
  allowedAgentProfiles:
    - name: implementation-fast
      goober: implementer
      harness: copilot
      model: configured-fast-model
    - name: review-deep
      goober: reviewer
      harness: codex
      model: configured-review-model
  allowedBases:
    - name: integration
      ref: refs/heads/integration
  allowPRPublication: false
```

Names above are illustrative; harness and model identifiers must resolve through
the configured host catalog. Profiles authorize exact tuples, not the Cartesian
product of independently allowed harnesses and models. Each profile's Goober
must also appear in `allowedGoobers`. Names are unique, closed-schema objects;
limit each list to 32 entries and reject blank or unknown references. There are
no credentials, executable paths, CLI flags, environment variables or provider
endpoints in these declarations. A Goober's pinned default harness/model remains
available when that Goober is allowed and ordinary admission succeeds.

Add a DSL 3.1 agentic-task field `agent: {harness, model}` to represent an explicit
selection in generated Workflow source. It changes only those two effective
Goober settings: instructions, capabilities, skills, required policy actions,
limits and integrity rules still come from the existing pinned definition.
The compiler applies the same host harness/model resolution used for configured
Goobers. Child admission additionally requires either the exact pinned default
or one exact declared profile. Deterministic tasks reject `agent`. Older DSL
versions reject these new fields. Effective execution definitions and the
selection participate in compiled and policy digests; runner adapters cannot
apply an invisible override after compilation.

Add an optional Workflow field `spec.childBase` with either
`{type: parent}` or `{type: named, name: integration}`. It is valid only in an
agent-authored child and never changes the normal configured scheduler catalog.
The name resolves against the pinned parent's `allowedBases`. Allowed refs must
be fully qualified branch or tag refs in the same admitted repository; no URLs,
revision expressions, arbitrary paths or cross-repository selection. Resolve a
named ref to a commit/tree under host repository authority before acceptance,
then store that identity in the immutable start envelope. Retries use the same
accepted commit even if the ref moves. Current repository access is still checked
at launch. Parent snapshot remains the default and includes existing permitted
uncommitted content under the child workspace contract. Named bases use the
resolved tree, without silently overlaying parent edits.

Defaults preserve existing behavior:

| Field | Omitted value | Validation and meaning |
| --- | --- | --- |
| `maxChildren` | 4 | Existing lifetime count per occurrence, hard ceiling 32; retries do not consume another invocation. |
| `maxConcurrentChildren` | 1 | Integer 1..32, no greater than effective `maxChildren`; counts accepted children whose disposition/custody has not been acknowledged. |
| `questions` | `human` | Closed enum `parent`, `human`, `parent-then-human`. This routes questions, not credential grants. |
| `control` | `autonomous` | Closed enum `autonomous`, `review`, `approve-each`. |
| `allowedAgentProfiles` | empty | Only the allowed Goober's admitted pinned defaults. |
| `allowedBases` | empty | Only the parent snapshot. |

Every new API field requires schema, deep-copy and generated manifest updates,
compiler validation, pinned-envelope/digest coverage, and authoring-catalog
coverage. An unavailable backend must reject the feature before acceptance;
schema acceptance alone never advertises executable parity.

## Shorthand compilation and selection

`start_helper` accepts `invocationKey`, `task`, `goober`, optional `harness`,
`model`, and `base`. Omitted harness/model inherit the selected Goober's pinned
defaults. A partially specified selection is resolved with those defaults and
must match a declared tuple or the full default tuple. There is no implicit
model substitution or cost-based provider fallback. Guidance prefers Copilot,
then Codex, then Claude when an authorized choice satisfies the task; the host
never turns that preference into additional authority.

Bound task text to 16 KiB UTF-8 and all names to the corresponding DSL identifier
bounds before parsing or allocation. Empty task/Goober/key, unknown fields and
ambiguous names are rejected. Task text is serialized as data using the YAML
encoder; it cannot inject a second task, document, capabilities or instructions.
The host produces one DSL 3.1 Workflow with one manual trigger, one agentic task,
no recursion, selected `agent`/`childBase`, explicit attenuated capabilities and
no delegated publication grant. Generated names are deterministic opaque names
from invocation identity, never raw task text.

Validation and submission share a compiler function; shorthand does not launch
a process itself. Retain exact generated YAML and its source/canonical digests,
compiled workflow and effective-definition digests, policy/config generation,
resolved base identity and immutable request digest. The ordinary start path
atomically records proposal references, child lineage, count reservation and
launch outbox entry. Repeating the key and identical request returns the same
receipt even after restart; changed input conflicts. Durable pending approvals
also own this key, so a moving branch or changed configuration cannot change an
already reviewed request. Before generating new bytes, look up that identity.

A receipt distinguishes `awaiting-approval`, `accepted`, `queued`, `starting`,
`running`, `waiting-question`, `result-ready`, and resolved terminal outcomes.
Acceptance is not evidence of process start. Existing child state remains the
source of truth; status labels are a projection, not a second lifecycle database.

## Parent wire and questions

`api/v1alpha1/operator_message.go` and
`internal/journal/operator_message.go` already provide bounded requests,
idempotent acceptance, acknowledgement and terminal outcomes.
`internal/engine/operator_message.go` sends reference-only terminal receipts to
Temporal. Reuse those content/receipt and scrubbing primitives, but do not
mislabel an agent as a human operator or treat terminal receipt delivery as a
mailbox. A versioned agent-message envelope adds authenticated sender lineage,
direction, sequence, question ID/revision and causal message reference.

| Tool | Authorized caller and effect |
| --- | --- |
| `message_parent(key, content)` | Child attempt; parent destination is derived from lineage, never a caller-provided run ID. Returns durable message receipt. |
| `ask_parent(key, content)` | Child attempt; atomically records a question and child wait intent, then applies the configured routing policy. Returns question receipt; transport disconnection does not withdraw it. |
| `reply_child(key, invocationKey, content, questionId?, expectedRevision?)` | Current parent occurrence; sends guidance or resolves one owned open question by compare-and-swap. A question answer requires both ID and revision. |
| `list_helpers(cursor?, limit?)` | Parent occurrence; returns paged owned children and pending message/question references with a durable continuation cursor. |

Messages are at most 8 KiB UTF-8 inline; larger content uses bounded authorized
artifact references. Initial ceilings are 128 pending messages and 16 open
questions per child, additionally bounded by tenant storage/rate quotas. Capacity
exhaustion returns a retryable closed-set error without silently dropping content.
Reserve bounded space for terminal receipts and cancellation so a full mailbox
cannot prevent cleanup. Ordering is per destination sequence, not global wall
clock. Accepted, acknowledged, delivered and answered remain distinct states.
Acknowledgement means included in a durable continuation input; delivered never
asserts that a model understood or obeyed the content.

The daemon ledger atomically accepts a message, records its delivery outbox and,
for questions, its wait intent. Journal projections use stable event identities
and replay idempotently. A crash between acceptance and notification leaves an
outbox item to retry. Deduplication compares canonical content and identity;
changed content with the same key conflicts. A stale attempt cannot send new
messages or answer questions. Previously accepted receipts remain readable under
current lineage authorization. Neither sibling children nor arbitrary runs may
address each other through this wire.

A parent waiting on children may wake for accepted messages/questions, inspect
all pending inputs, send replies, start more helpers up to its limits, or wait
again. It does not need a child's terminal result to resume. Before a parent
stage succeeds, all children must be disposed or placed in an explicitly
supported retained terminal state; outstanding questions are never lost through
a stage transition. Harnesses without live input delivery resume through the
normal stopped-attempt continuation path. Exactly-once external/model execution
is not promised; durable receipts and reconciled input delivery are idempotent.

Question routing is independent of control:

- `parent`: notify/resume the parent and wait. Parent unavailability alone cannot
  silently route to a human. Parent cancellation terminalizes the question.
- `human`: use the existing human intervention surface directly.
- `parent-then-human`: first durably notify the parent; escalate after a durable
  five-minute deadline, explicit parent escalation, or terminal parent failure.
  Restarting a daemon or a transient worker outage is not terminal failure.

Escalation changes the question revision and authorized answer route atomically.
Late parent answers then conflict; human and parent races produce one committed
answer. Cancellation, expiry and terminal child state similarly fence replies.
Answers carry provenance and remain untrusted task input. An answer cannot
approve a credential escalation, bypass a human-only approval, or change pinned
DSL. Content is scrubbed before retention; credentials and raw idempotency keys
never appear in Temporal history, metric labels or diagnostics.

## Human control and result review

`autonomous` allows parent starts and disposition decisions within policy.
`review` holds each new child start for human approval of its exact generated
DSL, selected Goober/harness/model, resolved base, capabilities and cost limits.
It maps to ask-before-start, not an implicit requirement for a human to accept
every result. `approve-each` additionally holds parent-requested merge, replace,
discard, and explicit disposal for human approval; stretch wake requests also
require approval because they start execution. Status reads, messages and
questions do not require approval. Safety cancellation and fencing never wait
for approval, but cancellation does not authorize deleting retained work.

Approvals live in the durable intervention ledger, not in MCP memory. Bind them
to tenant, parent occurrence, operation digest, policy generation, expected
workspace/result revision and expiry. A changed operation requires new approval.
Human approval cannot lift access ceilings; revalidate revocations and reserve
capacity at execution. Pending approvals consume bounded pending-request quota,
not execution capacity. Denied/expired requests retain receipts; no response is
never approval. Automatic retention cleanup follows the configured retention
policy only after safe disposition and is not a new parent dispose operation.

For both autonomous and reviewed results, use existing exact-revision
merge/replace/discard. A named-base child may be divergent from the parent:
three-way merge uses recorded ancestry, replace explicitly adopts its tree,
and neither operation assumes that its base equals the parent's snapshot.
Concurrent result application is serialized under parent workspace custody;
a parent edit or another accepted result invalidates stale preconditions.
Conflicts retain both results for a fresh parent decision. Completion of an
agent or approval of a request alone never releases writer custody.

## Local and distributed semantics

This table specifies the target contract, not the current local execution
availability. At the inspected main checkpoint, the local Runner coordinates
contained child-enabled stages: `cmd/goobers/parentpod_policy.go` requires worker
authentication and Claude/Codex parent profiles, and
`AttachParentAuthoringSkill` has the same parent harness restriction.

Consume the [trusted-local provider design (#7571)](https://github.com/Agent-Clubhouse/Goobers/pull/7571)
and its implementation rather than introducing another local backend flag.
That provider is selected only for self-only runner inventory with engine
projection disabled; an unavailable distributed runner never implies local
fallback. It retains `BackendRunner`, source-bound factories, private credential
homes, managed attempt worktrees and exact writer-quiescence evidence. Physical
local child invocations serialize by default through a shared permit, separately
from the concurrent lineage allowance below. Uncertain writers retain custody
on daemon loss. This design does not weaken that shared-account trust boundary.
Copilot parent admission and authoring-skill support are a separate prerequisite
for Copilot-based local end-to-end qualification; shorthand cannot establish
that support by choosing a harness string.

| Boundary | Local daemon | Temporal and Kubernetes |
| --- | --- | --- |
| Authoritative acceptance | Shared transactional ledger plus durable launch/delivery outbox | Same admission service and ledger contract; workers cannot accept independently. |
| Execution claim | Fenced daemon claim tied to exact child attempt | Activity/pod claim tied to the same child attempt; duplicate scheduling cannot create another writer. |
| Waiting | Persist wait/continuation before stopping the harness and releasing runnable capacity | Persist equivalent continuation; workflow wait uses reference notifications and releases worker/pod capacity after custody surrender. |
| Notification | Ledger-backed wake reconciler; lost in-memory wake is retried | Authenticated update/signal carries only bounded receipt references; activity reads authorized content. Replay never redelivers a harness side effect directly. |
| Questions and deadlines | Durable question CAS and persisted deadline | Same CAS authority; deterministic timer triggers idempotent escalation, including after replay. |
| Approval | Shared intervention service rechecks generation and exact digest | Same decision receipt; no pod-local approval cache grants execution. |
| Recovery | Rebuild outbox, reservations and waits from committed state | Reconcile workflow state against committed receipts; duplicate/lost notification is harmless. |
| Workspace | Isolated worktree with writer-stop and result evidence | Portable workspace/blob custody and stopped-pod evidence before handoff. |

There is no cross-system atomic transaction with Temporal. Commit the ledger and
outbox first; deliver an idempotent update and mark the outbox delivered only
after its acknowledgement. A signal is a wake hint, never the only durable
copy of a message. Add a versioned parent-wire receipt/update contract rather
than passing nonterminal mailbox states into the existing terminal-only operator
message update validator. Terminal or missing workflows leave a visible
reconcilable outcome, not a successful delivery receipt.

## Capacity, isolation and audit

The current one-unfinished-child check counts lineages without acknowledgement.
Replace it with the explicit concurrent bound inside the same acceptance
transaction as lifetime counting, lineage insertion and queue reservation.
Accepted/queued/running/waiting-question children and results awaiting disposition
all count. Stretch sleeping children also count. Terminal status alone does not
free the slot; acknowledgement after custody/disposition does. A duplicate
invocation returns its prior receipt before capacity checks. No polling or
check-then-insert sequence outside the transaction may admit a child.

Separate these retained-child slots from runnable CPU/pod/model concurrency.
Waiting parents and waiting children release runnable slots only after their
writers have stopped; their lineage/workspace reservations remain. Reserve a
bounded continuation lane for parent replies, cancellation and result disposal
so saturated child queues cannot deadlock the controller needed to release them.
Parent and child start claims require leases/fencing and startup reconciliation.

Host policy bounds tenant and run queued/runnable counts, retained workspace
bytes, mailbox/artifact bytes, request rate, elapsed time and model usage.
Parent DSL can only reduce these limits. Queue scheduling uses per-tenant fair
turns and capped bursts, rather than a single unlimited parent FIFO. Limits and
fairness must apply across daemon restarts and distributed workers; per-process
counters are insufficient. Unknown tenant policy or exhausted completion reserve
fails closed before acceptance. Never borrow another tenant's credentials or
reserved completion capacity to make progress.

Authorization binds tenant/gaggle, parent run, stage occurrence, child invocation
and current attempt. Every content/status/artifact read verifies that binding.
Delegated credentials are issued for the effective capability intersection and
selected harness/provider, scoped to the admitted repository and lifetime.
Do not forward parent environment blocks, ambient host credentials or private
runtime configuration to a helper. Deny a harness/model selection if available
credentials or sandbox/egress enforcement cannot honor its ceiling. Launch,
resume and result application recheck revocations; approval and pinned snapshots
do not resurrect revoked access. Local process isolation is not a hostile-tenant
sandbox: mutually untrusted tenants require OS/container isolation or refusal.

Audit at least proposal compilation/denial, approval requested/answered/expired,
child acceptance/claim/start, message acceptance/delivery/failure, question
routing/escalation/answer, wait/resume, capacity denial/release, result receipt,
disposition conflict/application, cancellation, custody transfer and cleanup.
Each event carries causal receipt ID, tenant and lineage IDs, immutable digests,
policy generation, actor class, timestamp and closed-set reason. Store content in
authorized scrubbed artifacts, not event labels. Audit refusals without reflecting
secrets or revealing foreign lineage existence. Metrics use bounded labels and
measure queue age, pending questions, outbox lag and reservation reconciliation.

## Optional sleep and wake

Sleep is requested, then confirmed only after stopping the exact writer and
persisting workspace/checkpoint custody. Until then the state is `stopping`,
not `sleeping`. Sleep releases execution capacity but retains its concurrent
child slot, workspace quota, pending task and unanswered messages. Waking rechecks
current authority/approval, reacquires execution capacity atomically and creates
a fenced attempt against the retained workspace. Task delivery has a durable
receipt; retrying wake cannot launch two writers. Harness conversation resumption
is optional and must fall back to a recorded continuation. Parent cancellation
and disposal supersede wake. Keep tools unavailable until both runtimes can
preserve this contract or explicitly reject unsupported placement at admission.

## Delivery and validation

Deliver focused PRs against the experimental integration branch:

1. This design, including review of policy spelling and control semantics.
2. Explicit task agent/base DSL, parent allowlists, schema/manifests, digest pins
   and normal admission; reject unsupported execution until runtime is wired.
3. `start_helper` compilation and MCP/HTTP integration through existing child
   acceptance; no direct process-spawn bypass.
4. Parent-wire ledger, authorization, receipt/outbox projection and local tools;
   durable parent/child continuation and question routing.
5. Temporal reference updates, replay and contained transport parity for the wire.
6. Control approval lifecycle, exact operation binding and local/distributed tests.
7. Atomic concurrent reservations, multi-child parent waits and serialized result
   disposition; tenant fairness and resource accounting qualification.
8. Optional sleep/wake only after the preceding custody contracts pass.

Features remain unavailable on any backend missing their enforcement. Update the
authoring catalog and reference when each executable slice lands; never silently
accept and ignore a policy field. No DSL lifecycle promotion is part of this work.

Hermetic tests must cover:

- Deterministic shorthand bytes, YAML injection, bounds, ordinary-DSL equivalence,
  declared/default tuple selection, invalid combinations, revoked credentials,
  named-base ref movement, retry identity and policy digest changes.
- Simultaneous starts at the limit (one transaction wins), duplicate retries,
  separate lifetime/concurrent counts, cancellation and acknowledgement, lost
  startup claims, queued-child accounting and saturated parent continuation.
- Message retry/conflict, restart between ledger commit and notification, stale
  attempt/foreign tenant denial, bounded overflow and scrubbed artifact access.
- Answer/escalation/cancellation races; expired approvals, stale workspace
  preconditions, policy reload and restart while a human decision is pending.
- Multiple active children and out-of-order results; parent wakes on a question
  while another child runs; merge conflicts preserve all custody.
- Temporal test-environment replay, duplicate/missing updates, activity retry,
  parent wait releasing capacity and contained-worker surrender; assert the same
  externally visible receipts and custody state as local tests.

Use targeted `go test -race -timeout 30m` packages, `make manifests` for API
changes, `make build`, and the repository CI aggregate. A local daemon journey
against a disposable test repository may use a fake harness for deterministic
coverage and Copilot for a separately reported live smoke test. Live success is
not a substitute for race, crash and isolation tests. Kubernetes qualification
requires a separately authorized disposable environment; no cloud provisioning
is implied by this design.
