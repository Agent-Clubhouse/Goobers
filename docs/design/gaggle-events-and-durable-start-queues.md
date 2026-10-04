# Gaggle events, durable start queues, and shared provider reads

Status: draft
Program: [HITL and advanced workflows](hitl-advanced-workflows-program.md)
Baseline: `04198152b63d228a9714ae2f92a7dca079ba5213` (2026-10-03)
Task prefix: `HAW-EVT`; task identifiers are stable planning identifiers, not GitHub issues.

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
Parent cancellation commits that fence before acknowledgement, then a recoverable cancellation outbox drives queued/live child cancellation; a separate cancel-receipt database is not an atomic acceptance fence.
Journal publication is recoverable from store transitions; an unavailable journal sink cannot erase accepted custody.
Read-model/SSE projection is asynchronous and rebuildable from retained control records; it is not the acceptance boundary.

## 5. Normalize every start source

| Source | Stable intent key and capture policy |
| --- | --- |
| Manual/API/CLI | Caller request key + authenticated initiator; validated target and inputs are pinned at acceptance |
| Schedule | Gaggle+workflow+schedule revision+nominal fire time; persist source cursor with captured occurrences |
| Backlog | Gaggle+workflow+qualified item+eligibility episode; one pending intent per episode, no per-poll duplicates |
| Event | Closed group identity, or delivery identity when debounce is disabled |
| Child | Parent run+stage occurrence (including branch/loop visit)+invocation key; retries reuse the same child acceptance |

A schedule's default catch-up policy is one coalesced start representing missed fire times within the prior hour.
The captured range/count is recorded; `all` is an explicit bounded policy, limited to 100 occurrences per catch-up sweep.
Future times are never pre-enqueued; changing a schedule establishes a new revision and cursor.
Backlog polling records source revision and eligibility evidence; dequeue revalidates eligibility and acquires the existing execution claim.
An item becoming ineligible before admission settles the intent as `obsolete`, with its reason recorded.
Discovery rotates paginated cursors; partial pages cannot be reported as a complete empty backlog.
If backlog demand currently lacks item identity, add identity-bearing candidate discovery; do not enqueue anonymous count-sized bursts.
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
