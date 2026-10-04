# Gaggle events, durable start queues, and shared provider reads

Status: draft
Program: [HITL and advanced workflows](hitl-advanced-workflows-program.md)
Baseline: `04198152b63d228a9714ae2f92a7dca079ba5213` (2026-10-03)
Task prefix: `HAW-EVT`; task identifiers are stable planning identifiers, not GitHub issues.

Implementation checkpoint: the local stack implements bounded receipts, pinned
route snapshots, configured same-gaggle subscriptions, shared storage reservations,
transactional routing/debounce,
host routing sweeps, exact archived local-runner consumer starts and reconciliation.
Both event dependency inventories protect journals and configuration archives
before routing is installed. See [receipt profile and limits](../reference/gaggle-event-receipts.md).
Public ingress, workflow publication, normalization of all start sources, queue
fairness/deadlines and engine/Temporal event input transport remain implementation
work. Workflow root budgets stay retained until a host proves all producers and
descendants settled. A journal-verified helper now preserves up to 32 roots across consumer
publication and refuses over-bound sets before acceptance; actual workflow
publication transport and child/continuation ancestry adapters remain pending.

## 1. Intent and confirmed boundaries

Every workflow start enters a durable queue: manual, schedule, backlog, event, and child workflow starts.
A successful intake response acknowledges durable custody of an intent; it does not promise immediate execution.
Workflows may publish events that match zero, one, or multiple consumers in their own gaggle.
Zero matching consumers is a successful publication with an inspectable disposition.
Consumers independently choose debounce keys and windows; original receipts and grouping history remain inspectable.
Accepted starts wait for ordinary eligibility, capacity, and budgets, subject to explicit deadlines and cancellation.
Events, event payloads, queues, and shared provider data never cross gaggle boundaries.
Provider reads are shared within a gaggle and the same authorized visibility scope.
Objectives and accepted backlog relationships remain in source repositories/backlogs; their index is rebuildable.
Durable execution intents, leases, journals, intervention decisions, and audit receipts remain Goobers control state.

Related designs:

- [Agent-authored child workflows](agent-authored-child-workflows.md): child acceptance, lineage, waiting, and cancellation.
- [Interactive factory operations](interactive-factory-operations.md): human identity, permissions, and portal actions.
- `source-owned-backlog-workbench.md`: portable declarations and source write-through.

## 2. Current implementation and reuse

`internal/triggerqueue` already durably accepts explicit trigger requests with actor/payload conflict detection.
Its states are accepted, dispatching, dispatched, and rejected; uncertain dispatch requires reconciliation.
`cmd/goobers/triggeracceptance.go` separates HTTP acceptance from daemon availability and preserves caller authority.
`cmd/goobers/triggerretention.go` prevents journal pruning from destroying evidence needed to reconcile an accepted start.
Extend these contracts instead of adding a competing start path.
`Scheduler.Signal` already broadcasts to zero/one/many subscriptions through normal admission.
Current signal delivery is best effort; admission skips do not create durable pending deliveries.
`goobers signal` takes the instance lock and is unsuitable as a live-daemon workflow publication mechanism.
The GitHub webhook receiver authenticates HMAC and uses bounded in-memory delivery-ID deduplication.
It currently supplies wakeups, rather than a durable generic event inbox.
`internal/apireadcache` supplies disk-backed GitHub conditional GETs and same-evaluation list coalescing.
The current cache lives under the instance scheduler directory and keys by credential fingerprint and request URL.
ADO WIQL and work-item hydration use read-only POSTs and already have batch support.
`ProviderQuotaState` supplies quota-window accounting; existing scheduler fairness, admission, claims, and idle backoff remain relevant.
The telemetry work-item API describes recorded Goobers actions; it is not a complete synchronized source backlog.

## 3. Public contracts and terminology

An **event** reports an occurrence without naming a required consumer or expecting its result.
A **start intent** requests one specific execution and identifies its target and acceptance policy.
A **delivery** associates one accepted event with one matching subscription.
A **group** collects deliveries for one consumer's debounce policy and produces at most one start intent.
A **run** is an execution admitted from a start intent; queue custody and execution ownership are distinct.
A child invocation names a target and awaits its result; it must not be implemented by publishing an event and hoping a subscriber replies.
Normal workflow runtime failures belong to the run's retry/HITL policy, not intake retries that silently create another run.

### 3.1 Proposed API shape

- `POST /api/v1/gaggles/{gaggle}/events`: publish; return HTTP 202 only after durable receipt commit.
- `GET /api/v1/gaggles/{gaggle}/events/{receipt}`: receipt, routing disposition, groups, and resulting intents.
- `POST /api/v1/gaggles/{gaggle}/starts`: accept an explicit target; existing trigger routes adapt to this service.
- `GET /api/v1/gaggles/{gaggle}/starts/{acceptance}`: custody, eligibility reason, lease state, and run reference.
- Queue list/cancel/retry operations use the same gaggle scope and existing authenticated operation policies.
- Existing `/api/v1/events` remains the read-model change stream; it is not the publication endpoint.

Acceptance returns stable `receiptId`, `acceptedAt`, `duplicate`, `statusUrl`, and a bounded `state`.
The event response can initially say `routing_pending`; zero-match success becomes `accepted_unmatched` after routing.
Authentication, invalid envelope, forbidden scope, and unknown explicitly named workflow fail before acceptance.
Storage failure/full capacity returns 503/429 with bounded Retry-After; it must not return an accepted receipt.
A transport timeout or ambiguous commit is an unknown outcome: retry the same key, never generate a fresh identity automatically.

### 3.2 Envelope and identity

Use the CloudEvents 1.0 JSON envelope: `specversion`, `id`, `source`, `type`; optional `subject`, `time`, `dataschema`, and `data`.
Keep transport content type, schema version, and Goobers extensions explicit; reject ambiguous duplicate fields.
Server-derived metadata includes gaggle, authenticated producer, source run/stage, root run, causation receipt, and chain depth.
Clients cannot override that authority metadata by adding equivalent fields to `data`.
External identities are namespaced by the configured ingress binding; a caller cannot impersonate another event source.
Each binding names exactly one gaggle and allowed event types; payload fields never choose the destination gaggle.
Reuse the hardened API listener, TLS policy, and named credential references; no second public listener is introduced.
Provider adapters verify signatures on bounded raw bodies; first-party publishers use scoped authenticated identities.
Default ingress is 10 requests/second with a burst of 50 per binding; reject excess before durable acceptance with Retry-After.
Content scrubbing and integrity classification run before persistence; audit rejected deliveries without copying secrets or unrestricted bodies.
Event idempotency scope is `(gaggle, producer binding, source, id)` with a canonical envelope digest.
The same identity and digest returns the same receipt; a changed digest or actor binding returns 409.
An event contains a bounded inline payload or verified artifact references, never credential values or unrestricted logs.
Artifact references must be accessible in the same gaggle and retained while pending deliveries need them.
An explicit start uses a caller-bound idempotency key; a child uses the durable parent invocation identity.
The child namespace is `(gaggle, parentRunID, stageOccurrence, invocationKey)`; attempt identity authorizes the caller but does not change the durable occurrence.
Derive the child start key from that namespaced tuple; do not overload `TriggerRequest.SourceRun`, whose existing meaning is a priority re-tick.

## 4. Durable data and transaction boundaries

Extend `internal/triggerqueue` and its existing durable database through schema migrations; do not introduce another independent start queue.
Every record carries a mandatory gaggle partition, and every lookup/mutation includes that partition in its identity and authorization.
The initial physical database may remain instance-wide under the existing admission authority; physical colocation does not authorize cross-gaggle data access.
SQLite remains the initial local/daemon backend; distributed workers call that service instead of opening independent copies.
Do not assume SQLite on shared network storage supplies a distributed scheduler lock.
The following logical tables may share a database; names are illustrative, constraints are normative.

| Record | Required identity and durable contents |
| --- | --- |
| Event receipt | Producer key/digest, envelope, authority, acceptance time, routing revision/cursor/disposition |
| Subscription revision | Immutable normalized filter, debounce policy, workflow target/version, scope and policy digest |
| Delivery | Unique receipt+subscription-revision pair, routing result, group membership, disposition |
| Debounce group | Consumer revision+key+generation, immutable first/last bounds, close reason, ordered delivery references |
| Start intent | Unique acceptance key, reserved run ID, source, target/config digest, inputs, policy, deadline, state |
| Dispatch lease | Intent ID, owner, expiry, monotonically increasing fence, last reconciliation result |
| Root-chain accounting | Gaggle+root identity, reserved/started counts, pinned limit policy, terminal dependent count |
| Transition history | Sequence, record ID, time, actor/service, previous/new state, reason, correlation references |
| Receipt tombstone | Identity+digest, final disposition, retained run/group references, expiry |

Acceptance atomically records the receipt and its routing work; it reserves enough accounting capacity to finish routing.
Routing atomically creates deduplicated deliveries in bounded batches and advances its cursor.
Routing has a default one-hour deadline; failure to complete routing settles remaining routing custody into an inspectable dead letter.
Already-created deliveries remain attributed; route completion, unmatched, and partial failure have distinct dispositions.
Membership changes and group closure are transactional; a closed group is immutable.
Group closure atomically creates one start intent and binds it to the group.
Run admission reserves one stable run ID before an executor is asked to start it.
Child acceptance atomically checks the parent cancellation fence, reserves the occurrence's unresolved-child slot and count, and creates lineage plus the ordinary start receipt in this same store.
It also checks the current stage-grant binding and retains the exact bounded proposal source in the same transaction. Proposal BLOBs are gaggle-scoped, content-addressed, and retained through their last full lineage owner; they share the existing byte reserve.
Parent cancellation commits that fence before acknowledgement, then a recoverable cancellation outbox drives queued/live child cancellation; a separate cancel-receipt database is not an atomic acceptance fence.
Journal publication is recoverable from store transitions; an unavailable journal sink cannot erase accepted custody.
Read-model/SSE projection is asynchronous and rebuildable from retained control records; it is not the acceptance boundary.

## 5. Normalize every start source

| Source | Stable intent key and capture policy |
| --- | --- |
| Manual/API/CLI | Caller request key + authenticated initiator; validated target and inputs are pinned at acceptance |
| Schedule | Gaggle+workflow+schedule revision+nominal fire time; persist source cursor with captured occurrences |
| Item-addressed backlog | Gaggle+workflow+qualified item+eligibility episode; one pending intent per episode, no per-poll duplicates |
| Count/refill worker starts | Gaggle+workflow+durable source observation+worker ordinal; bounded demand, with existing in-workflow scan/claim preserved |
| Event | Closed group identity, or delivery identity when debounce is disabled |
| Child | Parent run+stage occurrence (including branch/loop visit)+invocation key; retries reuse the same child acceptance |

A schedule's default catch-up policy is one coalesced start representing missed fire times within the prior hour.
The captured range/count is recorded; `all` is an explicit bounded policy, limited to 100 occurrences per catch-up sweep.
Future times are never pre-enqueued; changing a schedule establishes a new revision and cursor.
Item-addressed backlog polling records source revision and eligibility evidence; dequeue revalidates eligibility and acquires the existing execution claim.
An item becoming ineligible before admission settles the intent as `obsolete`, with its reason recorded.
Discovery rotates paginated cursors; partial pages cannot be reported as a complete empty backlog.
Legacy count-based scheduling has a different source unit: a worker start.
The current scheduler passes no item; a workflow's backlog-query stage scans and
claims work after the run starts (runner StartInput and ClaimLedger.ForRun).
Queue these starts under a bounded durable observation and ordinal, retaining
count/current-occupancy evidence without fabricating item IDs or item receipts.
This preserves the existing claim ledger, provider quota, no-work backoff, and
work selection behavior. Repeated polls must account for queued as well as live
workers so they cannot enqueue the same missing capacity repeatedly.
Identity-bearing discovery is required only for a source that promises to start
work on a particular item; it is not a prerequisite for these worker starts.
Repeated observation of an unchanged item does not create a new eligibility episode after a no-work run.
An episode changes only on a durable source transition, explicit retry, or an existing documented requeue policy.
Child starts use the same queue and admission; a parked parent occurrence releases execution capacity needed by its child. Runnable sibling accounting prevents a single waiting parallel branch from releasing the entire run permit.
Logical parent-run accounting remains visible; budget lineage includes all descendants and forbids budget reset through nesting.
The child design owns parent cancellation and result delivery; this store records the corresponding intent cancellation or admitted run.

## 6. Routing and per-consumer debounce

Subscription lookup is confined to the receipt's gaggle; there is no wildcard across gaggles.
At acceptance, pin the active routing-configuration revision; later config changes do not reroute already accepted events silently.
Filters initially support validated attribute equality and bounded boolean composition; data predicates require a declared schema.
Unknown event types may be accepted if the producer binding permits them; absence of matching subscriptions is normal.
Unsupported filter syntax is a configuration validation error, not an event silently treated as unmatched.
Each matching subscription creates an independent delivery; failure of one subscriber does not cancel the others.
The same event may be grouped by PR for one consumer and by repository for another.

Default debounce is disabled. When enabled, the consumer declares:

- `key`: a bounded typed expression over permitted event attributes/data; maximum 256 bytes after normalization.
- `window`: trailing quiet period; default 5 seconds, allowed range 100 milliseconds to 5 minutes.
- `maxWait`: absolute time since first member; default 30 seconds and never less than `window`.
- `maxEvents`: default 100, maximum 1,000; reaching the limit closes the group immediately.
- `inputMode`: `all` by default; `latest` is explicit and preserves every original receipt in group history.

A failed/missing debounce key becomes a failed delivery with an inspectable reason; it is never collapsed into a shared empty key.
Timers use server receipt time, not producer-supplied event time; order is receipt sequence then ID.
The deadline is `min(lastMemberAt + window, firstMemberAt + maxWait)`; persisted deadlines survive restart.
A transaction joining a group after its deadline first closes it and opens the next generation.
An event cannot join a closed/admitted group; groups never absorb events into an already running workflow.
`all` supplies bounded ordered references and summary metadata; the consumer hydrates authorized payloads on demand.
`latest` identifies its selected event explicitly; suppressed payloads remain traceable through the original membership list.
No debounce policy changes the event's receipt or pretends multiple occurrences were one occurrence.

## 7. Admission, fairness, and queue lifecycle

The initial states are `queued`, `waiting`, `dispatching`, `started`, `obsolete`, `cancelled`, `expired`, and `dead_letter`.
`waiting` carries a typed reason, next check time, and last observed policy revision; it retains custody.
Normal transitions are queued/waiting -> dispatching -> started; terminal intake states are immutable.
Capacity, provider quota, temporary readiness, paused workflow, and resettable budget exhaustion wait rather than reject.
Permanent policy revocation, unavailable pinned definition, invalidated inputs, or deleted target dead-letter with actionable evidence.
Existing explicitly authorized manual overrides remain explicit audited requests; no start source receives an implicit bypass.
Authorization is captured at acceptance and rechecked against current policy before dispatch; revocation cannot be bypassed by queue age.
Pinned workflow content remains immutable while current security/operating policy may tighten admission.

One coordinator reserves instance capacity atomically; gaggle queue partitions cannot independently oversubscribe it.
Use weighted round robin across nonempty gaggles, default equal weights, then workflows within a gaggle.
Within a workflow use acceptance order; configured priorities select classes, with one aging promotion every 5 minutes.
Priority cannot bypass budgets; cap consecutive admissions from one gaggle at 10 when another eligible gaggle is waiting.
Use existing workflow, gaggle, instance, placement, memory, claim, provider-quota, and spend checks at the final admission boundary.
Failed reservations are released; reservation expiry alone never authorizes duplicate execution.
Eligibility rechecks are event-driven when relevant state changes, with jittered 5-second minimum/60-second maximum fallback.
Evaluate at most 100 intents or 100 milliseconds per sweep; persistent cursors ensure deep queues make progress.
Unchanged waiting reasons do not append a history row on every sweep; record first occurrence, change, and hourly summary.
Public queue position is advisory because eligibility, fairness, and priorities differ; expose age and reason instead of a promised start time.

### 7.1 Deadlines and cancellation

Default maximum pending age is 7 days for manual, event, and backlog intents; schedule intents default to 1 hour.
A child is bounded by an explicitly configured parent overall or child-wait deadline; absent either, its logical wait has no implicit seven-day expiry.
Count/byte limits and intake backpressure bound indefinite waits; operators can inspect and cancel aged waits without a retention sweep silently dropping them.
Expiration terminalizes pending custody and notifies the source/parent; it does not delete the receipt immediately.
Workflow disable pauses new dispatch and leaves accepted work visible until re-enabled, cancelled, or expired.
Gaggle shutdown stops acceptance and drains/checkpoints leases; it does not cancel pending intents implicitly.
Cancelling a queued intent is atomic; cancelling during uncertain dispatch first reconciles and, if started, delegates to run cancellation.
Queue cancellation and run cancellation are separately reported so an accepted cancel is not mistaken for a stopped process.

## 8. Leases, crashes, and retry semantics

Dispatch leases default to 30 seconds, renew every 10 seconds, and carry a monotonically increasing fencing token.
Every dispatch-state write checks that token; an expired coordinator cannot commit a newer coordinator's state.
Local and Temporal starters must accept the reserved run identity idempotently before the durable queue is enabled.
Temporal start uses a stable workflow ID and an explicit no-duplicate reuse policy; local start verifies journal identity before resuming/acknowledging.
After a crash in `dispatching`, query the executor/journal for that exact run ID before any retry.
Observed matching run -> `started`; definitive absence -> retry the same identity; ambiguity -> remain in reconciliation.
A 24-hour unresolved dispatch ambiguity raises an operator-required condition and pauses further dispatch for that intent.
It retains evidence and custody; it is never expired, replayed, or pruned as though no run could exist.
If that occupies capacity limits, new intake backpressures until an operator reconciles it.
Fencing protects coordinator state; executor start idempotency protects run creation; provider mutation idempotency remains a separate responsibility.
The contract is at-least-once delivery attempts with deduplicated logical starts, not exactly-once external effects.

Retry transient infrastructure failures with full jitter, initial 1 second, cap 60 seconds, within the intent deadline.
Authentication/config/schema failures do not hot-loop; they dead-letter or wait for an explicitly identified reversible policy condition.
Dead-letter retry requires an authorized command, reason, and compatible policy; it creates a linked new delivery/intent generation.
The original terminal receipt remains unchanged. A publisher retry with the original event ID never replays completed consumers.
Explicit replay creates a new receipt with `replayOf`; it records selected consumer scope and chosen routing revision.
Replay cannot cross gaggles, alter source authority, bypass a budget, or recover an already executed child by minting a sibling silently.

## 9. Workflow publication and loop controls

Provide one deterministic `publish-event` built-in callable locally and through the engine activity seam.
Derive an emission key from source run, stage, branch, logical invocation, and author-declared occurrence key.
Stage retries reuse the key; separate intended occurrences require separate occurrence keys.
The producer records an outbox intent before sending and records the durable receipt afterward.
Crash between acceptance and producer acknowledgement resends the same event identity and recovers the receipt.
Advancing the stage requires an accepted receipt, including accepted-unmatched; it never waits for consumer completion.
Publish permission and allowed event types are declared capabilities; event publication grants no downstream mutation authority.
Consumers execute with their own declared permissions and budgets, with input integrity/provenance preserved.

Default chain depth is 8, maximum fan-out is 32 deliveries per event, and maximum resulting starts per root chain is 100.
These limits are enforced transactionally at routing/admission, not merely suggested in agent prompts.
Excess fan-out rejects the publication at validation when knowable; otherwise the receipt records a routing-limit failure with no partial hidden fan-out.
Reserve planned fan-out capacity against the pinned configuration before acknowledging receipt.
Depth/chain exhaustion records a terminal delivery reason and an operator-visible finding; it does not emit another triggerable error event recursively.
Origin/root/causation metadata is server-maintained; an agent cannot reset the chain counter by choosing a new event ID.
Unaffiliated external producers have per-binding quotas; internal run credentials cannot use that path to escape lineage.
Provider callbacks without trusted causation cannot inherit an invented root; workflow/gaggle rate and spend limits bound such indirect feedback loops.
Same-workflow self-subscription is allowed only when explicitly declared and remains subject to all bounds.

## 10. Bounded storage, maintenance, and observability

All limits below are initial operational defaults, configurable within documented hard ceilings.

| Limit | Default and behavior |
| --- | --- |
| Inline envelope | 16 KiB; larger data uses retained artifact references |
| Live intents | 10,000 per gaggle; intake backpressures without evicting accepted work |
| Unrouted receipts/deliveries | 10,000 receipts and 50,000 pending delivery memberships per gaggle |
| Open debounce groups | 1,000 per gaggle; excess intake waits before acceptance or receives retryable refusal |
| Ordinary terminal receipt/history retention | 7 days from last terminal dependent; original groups and memberships remain during that period |
| Child lineage retention | Terminal and acknowledged lineage: 30 days after acknowledgement/family settlement; child start receipt retained at least as long |
| Child lineage tombstones | Digest/identity/disposition only for a further 30 days; active family dependencies remain pinned |
| Dedup tombstones | 30 days, maximum 100,000 per gaggle; return duplicate/expired receipt, never silently replay within the window |
| Per-record transitions | 128 full transitions plus typed cumulative counters and first/last summary references |
| Control-store size | 256 MiB per gaggle; admission checks row/byte headroom and leaves maintenance reserve |
| Maintenance | Every minute, batches of 250 rows or 50 milliseconds; periodic bounded incremental compaction |

Retention is measured after all dependent deliveries/groups/intents settle; pending and ambiguous custody is never age-pruned.
Finite live-record/byte limits plus intake backpressure bound records that cannot safely be pruned.
Admission reserves estimated payload, membership, history, and terminalization headroom; maintenance capacity is not available to new publishers.
Reserve 20 percent of the store byte ceiling for transitions/reconciliation/pruning; payload-heavy acceptance stops at the other 80 percent.
Child lineage shares that byte reserve and has a 10,000-row live-lineage ceiling; do not add a competing five-percent reserve.
Storage exhaustion may still delay a transition; expose degraded custody and refuse new intake until recovery, without dropping old records.
Delete dependency leaves before receipts; release payload artifact pins only after their final dependent is retained/pruned safely.
Pin referenced subscription revisions and root-chain counters until the final dependent/tombstone expires; bound their creation through the same intake accounting.
Run-journal pruning must preserve evidence for uncertain dispatch and child result delivery, extending the existing trigger prune guard.
Tombstone limits backpressure new unique intake until expiry; do not evict a live dedup promise to make room.
After the advertised dedup window, clients must not blindly retry; status returns 410 while tombstones exist and 404 after removal.
Operators use explicit replay to request new work. Infinite deduplication is not promised.
Expose counts/bytes, oldest age, quota waits, unmatched events, group sizes, routing lag, retry causes, dead letters, and pruning lag.
Every list is cursor-paginated and gaggle-authorized; filters are indexed and never trigger an unbounded journal/provider scan.

## 11. Shared provider reads and source-owned indexes

First measure request volume by gaggle, workflow, endpoint, credential scope, page count, cache result, and retry cause.
Separate primary quota, secondary throttles, and ADO cost/delay; a lower HTTP count alone does not establish lower provider cost.
Create a per-gaggle read coordinator using existing provider models and capability declarations.
Coalesce compatible in-flight reads and bounded snapshots; keep query/representation/pagination semantics in the cache key.
GitHub retains proven conditional GET behavior, including the existing weak-validator safeguards.
ADO shares normalized WIQL read plans and batched hydration; cache only declared read-only POST operations, never arbitrary POSTs.
Reuse scoped source snapshots for different consumer predicates only when the snapshot is complete for those predicates.
Events mark objects/collections dirty; bounded delta reconciliation heals missed deliveries and polling remains an explicit fallback.
Keep last-successful-observation, revision, cursor/completeness, stale reason, and next-refresh time per scope.
Default foreground freshness target is 30 seconds; background reconciliation is 5 minutes with jitter and quota-aware backoff.
Stale data may support labeled browsing; admission, claims, and mutations revalidate the facts their correctness requires.

Cache identity includes gaggle, provider origin, repository/project, effective visibility scope, request shape, and authorization epoch.
There is no cross-gaggle cache reuse, even when credentials happen to match.
The portal's named interactive credential may have narrower permissions than automation; default to separate visibility partitions.
Only verified equivalent visibility permits reuse within the gaggle; token-reference name alone is insufficient proof.
Revocation/rotation invalidates visibility assumptions; search, counts, relationships, and SSE obey the same access boundary.
Omitted interactive credentials disable interaction; no request falls back to automation credentials.
Optional Goober instructions/binding guide assistance but cannot expand permissions or targets.
Write-through uses the configured interactive identity and records the initiating human; target scope remains the same gaggle.
Confirmed source writes invalidate affected records/lists, establish a minimum accepted revision, and prevent older polls from overwriting them.
If revision ordering is unavailable, use a refresh-generation fence and a post-write authoritative read.
Relationships/objectives are rebuilt from native backlog fields/relations and versioned repository declarations.
Store source provenance and revision pointers; preserve historical run snapshots separately from the current mutable source view.
Agents suggest relationships only during creation/curation; portal v1 browses and edits the owning source.

## 12. Rollout and compatibility

Land the store/service seams before changing scheduler behavior; initially support shadow comparison without dispatching shadow starts.
Use an explicit gaggle opt-in, proposed `execution.startQueue: durable-v1`, while compatibility evidence is collected.
Opt-in switches every source for that gaggle together; a child or manual route cannot bypass the queue.
The migration transaction adds/derives gaggle partitions and imports accepted/dispatching legacy receipts with actor, key, and reserved identity intact.
An ambiguous legacy gaggle binding requires explicit reconciliation; migration never assigns it to a broader default scope.
Mark the legacy drainer read-only before enabling the replacement; never run both dispatchers for the same acceptance.
Recover in-flight runs independently; an existing run is not re-enqueued as a fresh start.
Existing explicit requests keep durable acceptance and exit-code semantics; CLI wait defaults follow the receipt to a run/terminal custody outcome.
`signal` gains daemon-backed publication and no-match success; deprecate lock-bound publication only after compatibility coverage.
Schedule catch-up, temporary admission wait, queue expiry, and asynchronous event acknowledgement are documented behavior changes.
The intended end state is durable queues for all starts; remove the opt-in only after local/Temporal parity and migration gates pass.
Rollback drains or parks accepted custody under the new reader; older binaries must refuse unsupported queue schema rather than discard it.
Generic publication must not inherit the older unknown-signal-is-error wording from issue #648.

## 13. Implementation tasks and acceptance gates

| Stable task | Scope and meaningful verification |
| --- | --- |
| HAW-EVT-001 | Versioned envelope, scoped auth, receipt/store migrations; conflict/ambiguous-commit/full-store fixtures |
| HAW-EVT-002 | Shared start service and source adapters; manual/schedule/backlog/event/child all traverse one admission seam |
| HAW-EVT-003 | Routing revisions and consumer debounce; zero/multiple matches, independent keys/windows, restart boundary tests |
| HAW-EVT-004 | Idempotent executor start and fenced reconciliation; crash before/after executor start never creates another logical run |
| HAW-EVT-005 | Fair eligibility queue, cancellation, deadlines and budgets; fake-clock starvation, quota reset, parent-child capacity tests |
| HAW-EVT-006 | Workflow outbox publication and chain limits; retry identity, self-cycle, fan-out saturation, producer spoofing tests |
| HAW-EVT-007 | Production retention/pruners and inspection API; sustained saturation, dependency pins, tombstone expiry, bounded-query tests |
| HAW-EVT-008 | Gaggle-scoped GH/ADO read coordination; concurrent-consumer request counts, visibility separation, stale-write fences |
| HAW-EVT-009 | Legacy migration, shadow comparison, local/Temporal parity, docs/schema/examples and staged rollout evidence |

Required end-to-end scenario: three consumers publish/receive within one gaggle, one event is unmatched, two consumers debounce differently,
capacity is exhausted, the daemon restarts twice, one duplicate delivery arrives, and each eligible group produces one logical run.
Show every original event and grouping decision, pending reason, stable start identity, and final disposition through the API.
Repeat with child invocation, queue cancellation during dispatch uncertainty, narrowed interactive credentials, and provider throttling.
Verify a second gaggle cannot see events, records, counts, cached data, or artifacts from the first through any API/stream path.
Use synthetic providers and fault-injected local/Temporal fixtures; production monitoring measures rollout rather than supplies destructive tests.

## 14. References and remaining decisions

- [CloudEvents 1.0.2](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/spec.md): interoperable envelope, not delivery guarantees.
- [Knative brokers](https://knative.dev/docs/eventing/brokers/) and [triggers](https://knative.dev/docs/eventing/triggers/): ingress/subscription separation.
- [Argo sensor semantics](https://argoproj.github.io/argo-events/sensors/more-about-sensors-and-triggers/): explicit grouping and dependency behavior.
- Existing backlog: #648 generic intake; #368 PR events; #4430 durable health transitions; #5257 uncertain legacy trigger receipts; #5960 ADO hardening.

Before implementation, ratify configuration field names, numeric defaults, and whether explicit schedule `all` catch-up is needed in v1.
Resolve the shared capacity reservation storage seam with the child design; do not release parent execution capacity by falsifying run completion.
Keep initial filters small; temporal joins, arbitrary scripts, external event brokers, and cross-gaggle routing are outside this design.

### Delivered local publication slice (HAW-EVT-006)

The local daemon runner now supports the deterministic `inputs.kind: publish-event`
built-in in opted-in DSL 3.1 workflows. A task must declare `event:publish`, and its
gaggle must explicitly list its workflow and literal event types under
`spec.events.publishers`. Both the retained generation and the current applied
policy must allow publication. Engine execution, remote dispatch, child ancestry,
human continuations and same-workflow self-subscriptions explicitly refuse until
those transports and ancestry/opt-in contracts are qualified.

```yaml
# Gaggle spec
 events:
   publishers:
     - workflow: produce
       allowedTypes: [com.example.build.finished]
   subscriptions:
     - name: inspect-build
       workflow: inspect
       filter:
         all: [{attribute: type, equals: com.example.build.finished}]
```

```yaml
# Deterministic task in the produce workflow
- name: announce
  type: deterministic
  workspace: scratch
  goal: Publish the completed build notification
  capabilities: [event:publish]
  inputs:
    kind: publish-event
    type: com.example.build.finished
    occurrenceKey: build-completed
    data: '{"build":42}'
  run:
    command: ["true"] # Required DSL field; this typed kind launches no shell.
```

`subject` is optional. `data` is a JSON string because deterministic DSL inputs
are strings; the structured envelope is limited to 16 KiB. No caller-supplied
source, ID, actor, run, branch, root or destination gaggle is accepted. The host
binds the emission to its committed stage occurrence and branch. Retries retain
that occurrence and `occurrenceKey`; an intended new emission must use a distinct
logical visit/key. Payload changes under the same identity conflict.

A bounded durable outbox intent precedes receipt admission and pins the original
routing plan. Lost acceptance replies recover the same receipt, even after a
catalog change. Success includes `accepted_unmatched`, and task outputs contain
`receiptId`, `eventId` and receipt `state`; consumer completion is separate. Event
consumer publication verifies the retained consumer journal and carries every
original causal root forward. Current permission revocation refuses publication
before new outbox custody.

Producer outboxes retain journals, source and consumer configuration generations,
receipt deduplication, and the original consumer group/input ancestry. The daemon
releases this custody only after acquiring registry ownership and an exclusive
journal recovery lock, then verifying the exact completed/aborted run.finished
sequence. Seven days after that fence, it compacts the intent into a bounded
identity/digest/receipt tombstone for thirty further days. Exact observations
remain possible during that window; a tombstone cannot recreate or republish an
event. Maintenance bounds both compaction and expiry and reserves progress for
compaction while old tombstones expire.

Failed, escalated and interrupted producers remain resumable and retain custody.
Failed event consumers retain their original group and input receipts even when
they fail before their first publication, preserving existing ResumeFromTerminal
behavior. They need an eventual explicit irreversible disposition; elapsed time
does not implicitly abandon them. Whole-root budget settlement remains a separate
future proof that every producer and descendant has settled. The 10,000-active-
intent per-gaggle bound, 100,000 tombstone bound and shared byte quota fail intake
closed rather than evicting a live replay promise.

## Delivered ordinary start normalization: daemon manual ingress

New ordinary daemon HTTP requests and same-root file-delegated manual, targeted
PR, and priority requests now enter the existing `accepted-triggers.db` before
scheduler admission. Their typed `goobers.workflow-start/v1` envelopes retain the
original request, authenticated HTTP actor (or explicit local-file service actor),
resolved gaggle, exact applied configuration generation, workflow digest, and
Goober digest. Caller bodies cannot provide these execution pins. Exact retries
reuse original pins across successful or rejected configuration reloads.

The archive is leased before acceptance commits, pending and uncertain receipts
protect it from pruning, and execution holds a lease through its actual starter
lifetime. Dispatch reconstructs the archived normal local/engine starter and
rechecks current workflow eligibility, exact repository target, capacity and
cadence budgets. Targeted PRs retain their current subscription and provider
validation; manual force still cannot bypass concurrency. Ordinary engine starts
retain the existing durable-journal-before-execution contract; this does not
qualify event input transport on Temporal.

File delegation is now an ingress/response protocol, with the shared ledger
owning accepted custody. A persisted transfer marker protects the uncertain
acceptance window from false withdrawal acknowledgments. Lost replies and daemon
restart reuse the same key and reserved run ID. Existing file validation,
per-identity bounds, queued acknowledgments and final run responses remain;
`--no-wait` acknowledges durable file acceptance before provider validation;
final dispatch/status still reports a refused PR. Accepted file deadlines are
retained in the intent and provider validation has
its own bounded attempt context. Already uncertain legacy dispatch files keep
their original fail-closed recovery rule. Existing legacy HTTP receipts remain
readable and exact retries do not silently recapture them.

The remaining source adapters are explicit follow-up work:

| Existing source | Current path and remaining normalization |
| --- | --- |
| Standalone `goobers run` / detached one-shot worker | Delivered below: owns the instance lock, queues one pinned intent, and attempts only its own receipt. |
| Direct `engine-start` | Delivered below: typed shared-ledger custody preserves exact Temporal target and existing scheduler-bypass semantics. |
| Scheduled starts | Delivered below: pinned coalesced worker start/cursor and retained demand-sized obligations. |
| Backlog polling | Delivered below: bounded count observations become queued worker ordinals, preserving in-workflow item selection and claims. Item-addressed sources separately use eligibility episodes. |
| Desired-concurrency refill | Delivered below: live and queued occupancy bounds missing-capacity worker starts; normal admission and in-workflow claims remain. |
| Direct webhook/signal delivery | Delivered below: authenticated delivery/caller keys freeze the pinned recipient set before acknowledgement. |
| One-off / future portal starts | Must call the shared ordinary start admission service when implemented. |
| Events and generated children | Already use typed pinned starts in the same ledger. |

This slice is not the final all-source queue rollout. No separate queue is
introduced, and existing runs are not re-enqueued as new workflow starts.


### Delivered plain schedules and direct source delivery

The daemon now queues plain scheduled starts before execution. A shared-ledger
transaction stores the source receipt, exact archived recipient, and evaluation
cursor together. Cursor compare-and-swap, queue capacity, or archive failures
leave the previous cursor due. The existing trigger-evaluations file remains a
status projection. On first adoption, its historical baseline seeds the cursor;
an outstanding legacy fire is transferred once with a durable adoption marker,
so a crash before clearing the old file cannot resurrect it.

This compatibility adapter retains the current scheduler's one coalesced worker
per due evaluation interval across all declared schedules. Captured from/through
times and workflow/configuration pins preserve the accepted occurrence. It does
not implement the proposed one-hour lookback or configurable all-occurrences
catch-up policy above. Those remain an explicit policy change; the existing
catch-up semantics are preserved. Cursors are per gaggle/workflow, survive edits,
and count against shared bounded custody; deletion/recreation does not silently
reset them. Explicit retired-cursor cleanup remains follow-up work.

Authenticated GitHub webhooks retain delivery ID, full authenticated payload
digest, and repository/event metadata. Acceptance freezes all matching recipients
(or no-match) in one transaction, bounded to 32 starts. A retry after reload or
restart observes that original set; changed content under the same key refuses.
Queue failure returns unavailable rather than acknowledging a lost delivery.
The existing signature check, repository matching, and idle-backoff selection
remain. Source receipts and starts share queue slot/byte ceilings, with the
existing seven-day replay horizon; unfinished starts retain the source receipt
beyond that horizon. No source receipt is an item claim.

`goobers signal --request-id <key> <name> [path]` uses the same durable source
adapter while holding its existing standalone instance lock. Without the flag it
prints a generated key. It starts only its accepted recipients, waits for admitted
runs as before, and leaves capacity-held starts for `goobers up`; unrelated queue
records are not executed by this one-shot command. Live-daemon named-signal
forwarding is not added. Ordinary daemon sweep/reconciliation then uses the same
reserved run IDs, current eligibility and archived execution path as manual starts.

Acceptance tests exercise actual webhook handlers, exact archive compilation,
scheduler admission, Runner journals, and CLI key replay. Store tests cover
transaction rollback, queue-full cursor preservation, legacy transfer uncertainty,
and no-match/recipient freezing. The direct engine compatibility adapter is described below.
Demand-sized schedule custody is described below.


### Delivered count-based backlog and refill workers

When the daemon installs the shared source queue, a successful backlog count or
refill observation captures up to 32 worker starts per source, with exact applied
configuration/workflow/Goober pins, observation time, eligible count, and worker
ordinal. These are worker occurrences. No provider item ID or claim is invented;
the workflow still scans, selects, and claims work using its existing stages.
Provider polling cadence, quotas, authentication backoff, and existing refill
selection remain in place. A stale count may result in the workflow finding no
work, as it could before this change.

Before acceptance, live scheduler owners and queued starts reduce available
workflow capacity. Refill also respects desired concurrency. Pending ordinary,
event, and generated-child starts share this accounting, with children charged
to their configured parent workflow bucket. An exact live run ID is excluded
from queued occupancy only when it is already counted by scheduler admission.
Unqualified legacy starts conservatively occupy matching workflow names until
resolved. The queue transaction checks occupancy again, so a concurrent manual
acceptance cannot give two automatic observations the same missing slots.
Scheduler admission and the live-owner snapshot are coordinated while accepting
the bounded batch. Shared queue slot and byte limits remain unchanged.

After restart, pending starts continue to occupy those slots. Exact observation
replay keeps the first accepted starts and pins; changed counts under the same
observation key refuse. Archive or queue failures never fall back to direct
execution. Dispatch requires both the captured and current backlog/refill source
to remain configured and uses normal current capacity/provider/budget admission.
Capacity-held refill starts remain queued under the same reserved run identity.
Failed provider polls do not create an item or bypass execution-time claims.

Tests cover independent SQLite writers, atomic occupancy refusal, queued manual
and child/event budget owners, exact live-owner exclusion, repeated polling and
scheduler reconstruction, removed sources, archive failure, refill capacity
retry, and actual pinned runner execution before queue reconciliation.

### Delivered demand-sized schedule custody

The daemon records a coalesced firing and advances its durable cursor together
before polling demand. The obligation retains exact configuration, workflow and
Goober pins. Its counter is compiled from that archived generation; provider
polling still passes through scheduler priority, quota and authentication logic.
A successful count is sealed once, bounded to 10,000 worker occurrences. Restart
preserves that count and the next untransferred ordinal rather than recounting
against a changed definition. An unobserved failure retains its obligation;
existing timeout/transient fallback semantics still produce one queued worker.
An explicit zero closes the fire without launching a run.

Each tick transfers at most 32 workers and only the currently missing workflow
slots. The same transaction updates ordinal progress and accepts ordinary pinned
starts, including the queued/live occupancy guard. A crash cannot spend an
ordinal twice. Later due firings coalesce with existing outstanding custody;
they advance the schedule cursor without replacing the original pins or count.
Slot release wakes retained demand promptly, and unresolved obligations receive
bounded periodic retries. Legacy outstanding-fire markers transfer once when a
cursor is first adopted; stale legacy files cannot resurrect transferred work.

The shared retention inventory includes obligations before provider observation
and throughout transfer to ordinary starts. Demand IDs sort before start IDs,
so transferring ownership during pagination does not hide a generation pin.
Queue slot/byte exhaustion leaves the cursor unchanged. Removed or disabled
sources retain pending obligations until they can be explicitly resolved; they
do not launch under another workflow or lose pins through an age-based timeout.

Tests cover count preservation across database reopen and configuration change,
no-work/fallback decisions, concurrent ordinal transfers, rollback, queue-full
cursor preservation, failed pin capture, and actual archived Runner execution.
The direct engine compatibility adapter is described below.

### Delivered standalone manual and detached starts

`goobers run` without a live daemon and its detached worker accept the same pinned
ordinary intent into the shared ledger before normal scheduler admission. They
retain the existing instance lock, claim recovery, current targeted-PR validation,
force rules, trace handle, terminal exit codes and cleanup. The one-shot adapter
attempts only its own receipt; it never executes unrelated accepted starts.

`--request-id` now carries an exact retry identity through local and detached
submission. Exact replay observes the original receipt, source pins and run ID;
changed options under that key refuse. If current capacity holds the start, the
command prints its durable receipt and request ID and returns the existing
nonzero admission result. `goobers up` or a same-key retry can subsequently
dispatch it. Standalone `--no-wait` still returns after run admission; daemon API
submission-only behavior remains durable acceptance before dispatch.

Queue/archive failures cannot fall back to direct execution. A previously
uncertain publication is reconciled against the matching durable run journal
while holding the instance lock. Tests cover real CLI and detached-worker replay,
an unrelated pending receipt, archived execution after capacity release, queue
failure, current provider validation and original run/terminal output.

### Delivered direct engine compatibility custody

Explicit `engine-start --direct` (also the existing daemon-down default) now
accepts a typed receipt and canonical compiled `RunInput` into the same SQLite
ledger before calling Temporal. The input attachment is bounded to 1 MiB and
counts against the shared database byte ceiling; the receipt consumes the common
slot limit. Input and receipt are committed together, with cascading cleanup of
confirmed receipts after the existing seven-day replay horizon. Uncertain effects
never expire to make room for new work.

The direct path preserves its dedupe-derived run ID, frontend, namespace, task
queue, live-journal option, and explicit scheduler-bypass behavior. It does not
consume a scheduler slot or acquire ordinary terminal hooks. Full input bytes
pin definition, configuration generation, placement and policy. Credential
selector hashes preserve TLS/codec binding without storing materialized secrets;
relative file selectors retain their original base directory. Dispatch resolves
only currently matching configured selectors. Changed options refuse rather than
retargeting an accepted receipt.

The attempted marker is durable before the provider call. New starts stamp the
canonical input digest in Temporal memo. Reconciliation reads one bounded first
history event with the configured payload codec and verifies the actual input,
workflow type and task queue, plus the digest memo when present. This also allows
an exact legacy history without the new memo to prove its original input. A
missing, deleted, mismatched or undecodable history after an attempted call never
authorizes a resend. The receipt remains uncertain; explicit operator disposition
of permanently unavailable evidence is future work. A dial/configuration refusal
before the attempted marker leaves the receipt accepted for retry.

History verification inspects event 1 within a bounded first page, allowing later
events from the same persistence batch. Temporal's history manager expands whole
batches, so a requested page size of one is not an exact event-count guarantee
([server implementation](https://github.com/temporalio/temporal/blob/main/common/persistence/history_manager.go)).
The adapter caps the returned page at 4 MiB and 4,096 events before decoding the
accepted start input; it never scans later pages to manufacture a match.

The daemon uses a separate bounded cursor so an unavailable direct target cannot
take over ordinary pending batches. Direct receipts cannot enter the ordinary
name-based launcher or count as scheduler worker demand. Their retained generation
pins join pruning inventories; confirmed external histories keep the pre-existing
external-generation ownership marker. Automatic expiry of those external markers
remains outside this adapter.

Tests cover actual CLI capture, exact output/dedupe replay after source changes,
daemon recovery of lost replies using the sealed codec, credential-selector
revocation, missing/mismatched history, concurrent admission, atomic input custody,
migration, and retention. A composed delegated `engine-start` test also proves its
file request becomes a pinned ordinary receipt before acknowledgement and executes
that archive after authored-source changes. Delegated backend selection remains
the daemon's existing scheduler decision, including its local-runner fallback.


### Delivered scoped read-cache foundation (HAW-EVT-008)

The shared provider GET cache now accepts an explicit gaggle, source/credential
binding, and configuration or interactive-policy generation. It hashes the
complete scope, endpoint, credential and representation headers. Missing scope
bypasses caching; equal credentials alone cannot share bodies across gaggles.
Ordinary stage GitHub/ADO reads and snapshot invalidation use an explicit
executor-owned automation binding and the trusted invocation's pinned generation.
Gaggle/config environment alone never enables caching. Unknown, unpinned, human
continuation, and isolated execution paths without an explicit binding bypass
sharing. The shell launch removes authored/ambient cache-binding overrides;
ordinary host construction supplies the supported binding. Human CI providers
remain uncached, and model-only shared sessions do not enter this provider plane.
An interactive cache requires a separately verified binding and current policy
generation before it can be enabled. Callers still authorize every read before
invoking the cache; cache custody grants no access.

The reusable client supports GitHub and ADO GETs, preserves configured transports,
coalesces list reads within a bounded evaluation snapshot, and retains GitHub
Link or ADO continuation headers. Bodies larger than 16 MiB stay streaming and
uncached. Explicit fresh/conditional/range requests, no-store responses and
unscoped reads bypass shared replay. Existing strong-validator revalidation and
bounded SQLite retention remain. Snapshot invalidation affects only its scope.

Concurrent fake-transport tests verify one ADO list request for eight consumers,
credential/gaggle/generation/representation separation, pagination, large-body
streaming, and production stage scope/invalidation wiring. These tests require no
provider service or socket listener. Daemon backlog/refill/remediation counters
now receive the generation captured by scheduler construction. Open-PR refreshers
include gaggle, binding, and generation in both provider-cache and in-memory
refresher identities, even when two gaggles use the same repository and token.
Their GitHub and ADO credentials are still resolved/scrubbed on each provider
operation; quota decorators and conditional refresh behavior remain intact.
Tests exercise actual configured ADO auth, credential rotation, retained scheduler
pins, ordinary shell launch, and no cross-gaggle in-memory refresher reuse.
ADO WIQL read-plan coordination and batch hydration remain follow-up work. This
slice does not claim every provider read is centralized.
