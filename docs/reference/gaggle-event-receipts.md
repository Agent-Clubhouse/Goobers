# Gaggle event receipts and durable routing

Status: internal receipt/routing and local-runner host implementation for
HAW-EVT-001/003/004/007 and the HAW-EVT-006 root budget. Public ingress, workflow
publication and normalization of every start source remain implementation work.
The daemon can drain accepted internal receipts into pinned consumer runs; these
store APIs do not expose a public event endpoint.

The existing durable trigger database now accepts internal event receipts. Intake
commits the authenticated producer binding, gaggle, bounded event envelope and
matched routing snapshot together. Transport adapters must construct authority
from authenticated configuration, scrub secrets before intake, and enforce ingress
quotas and derive causal lineage. Payload fields never select a gaggle or producer.

## Envelope profile

Goobers uses the [CloudEvents 1.0 JSON envelope](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/formats/json-format.md)
with `specversion`, `id`, `source` and `type`. This initial profile accepts JSON data
and scalar extension attributes. It refuses binary `data_base64`, server-owned
`goobers*` extensions, duplicate keys, ambiguous casing, invalid context attributes,
more than 32 nesting levels, and envelopes larger than 16 KiB. Large data belongs
in separately authorized artifact references. Extensions are retained, not used as
authority. Attribute replacement characters are refused as an additional profile
restriction; arbitrary CloudEvents transports are outside this initial profile.

Normalization orders object keys and removes insignificant whitespace. It keeps
JSON number spelling and precision: `1` and `1.0` are different payloads. The
digest identifies normalized bytes; it is not a claim of RFC 8785 canonical JSON.

## Subscription matching

Each immutable gaggle catalog contains at most 128 subscriptions, with at most 32
matches per publication. Filters support equality on `id`, `source`, `type` and
`subject`, plus bounded `all`, `any` and `not` expressions. Payload data cannot
select authority or run expressions. Each match pins its consumer revision,
workflow/Goober digests and retained configuration generation.

Debounce uses one explicit envelope attribute or constant key. A missing key
records a failed delivery without failing unrelated consumers. Configured filters
are bounded by depth and node count; compiled catalogs copy definitions so later
caller edits cannot change accepted matching behavior.

### Gaggle declarations

The gaggle schema accepts subscriptions under `spec.events`. Configuration
validation requires every target workflow to exist in the same gaggle. At
acceptance, the trusted loader resolves its retained workflow, Goober and
configuration digests; declarations cannot override these pins.

```yaml
spec:
  events:
    subscriptions:
      - name: review-updated-pr
        workflow: review-pr
        filter:
          all:
            - attribute: source
              equals: urn:example:pull-requests
          any:
            - attribute: type
              equals: com.example.pr.updated
            - attribute: type
              equals: com.example.pr.ready
        debounce:
          keyAttribute: subject
          window: 5s
          maxWait: 30s
          maxEvents: 100
          inputMode: latest
```

The first declaration profile requires every `all` condition plus at least one
`any` condition when that list is supplied. Either list can be omitted; together
they must contain a condition. Each list allows up to 15 conditions, with literal
`attribute`/`equals` comparisons and optional `not: true`. A negated equality also
matches when an optional attribute is absent. Arbitrarily nested declarations and
payload predicates are outside this first profile.

Omitted debounce is disabled. When present, declare exactly one `keyAttribute`
or `constantKey`. Defaults are `window: 5s`, `maxWait: 30s`, `maxEvents: 100`, and
`inputMode: all`; an explicitly longer window needs a compatible maxWait. A
consumer revision includes the effective defaults, filter and resolved source
pins, so a changed generation cannot accidentally join an older group.

Declarations currently validate and compile; production publication still requires
the pending host integration described above.

## Custody and retries

Identity is `(gaggle, producer binding, source, id)`. A matching payload and actor
recovers the original receipt, acceptance time and routing snapshot even after
subscriptions change or the daemon restarts. A changed payload or authority
conflicts. Retry an uncertain outcome with the same identity. A new identity is a
new publication.

An empty matched route snapshot produces `accepted_unmatched`, a successful
terminal receipt. Matched events retain `routing_pending` custody. The store
reserves ordinary start slots, payload and maintenance space for up to
32 matched consumers before acknowledging intake. Existing manual/child intake
counts those reservations, so it cannot spend event completion headroom.

Routing that has not completed within one hour becomes `routing_failed` with a
reason. This is not a failed consumer run. `RouteNextEvent` atomically moves one
receipt's reservations into deliveries, groups and queued starts. Storage failure
rolls back the whole transaction for retry. A missing debounce key or exhausted
root budget records a failed delivery while preserving other consumer deliveries.

## Pinned routing and debounce

Routes pin a configuration generation, workflow digest, Goober digest and consumer
revision. Routing never looks up current subscriptions. Legacy receipts lacking
generation pins fail visibly instead of being repaired from the current catalog.
Each consumer has an independent key/window. Server receipt sequence orders
membership; windows use server receipt times, never event payload times. A group
closes at its quiet deadline, maximum wait or maximum event count. Due timers wait
for earlier accepted receipts to route, so delayed routing preserves membership.

`all` exposes every ordered input; `latest` selects the final receipt while keeping
all suppressed memberships inspectable. Queue payloads contain bounded group
references, not an inline payload array. `EventMembers` pages at most 100 references;
`EventInput` requires same-gaggle group membership. `VerifiedEventStart` verifies
that queued bytes still equal the immutable group and its exact pins. The typed
`goobers.event-start/v1` request is routed by the daemon to the pinned event launcher, never to a
current-catalog workflow alias.

The transactional root budget permits 100 starts per event chain; a coalesced
group charges each represented root once. Workflow producers must retain their
server-derived root ID. Independent external receipts start separate roots.
External-root budgets can expire with dependent history. Once workflow producers
join a root, its counter and producer journal references remain until a future
host acknowledgment proves every producer and descendant settled. Receipt expiry
cannot reset that budget. Those retained counters share the bounded database and
can backpressure intake; host root settlement remains required follow-up work.

## Bounds and maintenance

Full receipts are limited to 10,000 per gaggle, tombstones to 100,000 per gaggle,
retained deliveries to 50,000 per gaggle and open groups to 1,000 per gaggle.
Pending reservations count toward group/delivery and shared 10,000-start quotas,
and all receipts share the existing instance database's 256 MiB hard ceiling and
20 percent maintenance headroom. Physical colocation does not grant cross-gaggle
read access. The initial physical byte ceiling is instance-wide, so one gaggle
can cause intake backpressure for another; per-gaggle fairness remains queue work.

Completed unmatched/failed receipts retain payloads and routes for seven days.
Delivered receipts retain those bytes until seven days after the last consumer
settles. Queue dispatch acknowledgment is insufficient: the host must observe a
terminal journal or durable start rejection and call `SettleEventGroup` with the
exact consumer identity. Group, membership and start references conservatively
remain through the receipt's thirty-day identity/digest tombstone period. Retry
during that window recovers the tombstone and cannot silently republish. After
tombstone removal, infinite deduplication is not promised. Full tombstone quotas
backpressure intake rather than deleting a deduplication promise early. Maintenance
is bounded to 100 custody work units and a 50 ms context per daemon sweep, including when no
scheduler is attached. `EventDependencyPage` exposes source/consumer journals and
generation pins from receipt acceptance onward; `EventRootDependencyPage` adds
long-lived root producer references. The host must successfully collect both
bounded inventories before pruning those resources. The existing host journal
guard and generation-pruning inventory now include both dependency sources,
including before daemon composition. Intake must hold the accepted generation's
lease through receipt commit. Without consumer settlement, accepted inputs and
starts remain retained.

## Host execution and recovery

The daemon routes one oldest pending receipt per selected gaggle, closes due
groups, and pages settlements with rotating cursors. Routing/settlement work has a
one-second sweep context. An error in one accepted start does not stop dispatching
other starts in the same bounded batch. This is not yet a full fair eligibility
queue; blocked entries and future cancellation/deadline behavior remain follow-up.

The launcher acquires the accepted configuration archive and compiles it through
the ordinary daemon builder. Workflow and Goober content pins must match exactly.
The currently configured same-gaggle workflow must remain enabled and eligible,
with the same repository target. Normal scheduler concurrency, provider limits,
hour/day budgets, run tracking and shutdown joins apply. Mutable pending source
edits cannot replace accepted definitions. The archive lease spans execution.
Unsupported engine/Temporal consumers remain accepted with an explicit launch
error until their event-input transport is implemented; they are never run using
an alternate backend or a current-name fallback.

Before claiming a start, the host verifies original group membership and canonical
payload digests, and refuses bytes that require intake scrubbing. The run records
trusted `event-consumer-manifest` provenance plus selected `event-NNNN.json`
payload snapshots at `unapproved` integrity. A signed producer does not raise
payload integrity. `latest` retains every producer and original membership in the
manifest. Inputs and matching context pointers are immutable journal snapshots.

The reserved queue RunID survives retry and crash. A returned scheduler admission
is still `dispatching`; only the exact journal identity, input manifest and receipt
membership acknowledge dispatch. Live-process absence cannot authorize retry.
Startup-owned uncertain custody with strong journal absence can retry the same
RunID. Recovery recompiles the same retained generation and checks current
eligibility; missing or mismatched evidence defers that event run. Terminal
settlement requires a joined owner, terminal phase and matching `run.finished`
evidence. Human-paused, escalated and interrupted runs retain their inputs.
Human continuations retain copied inputs and source ancestry but receive their own
execution identity, clearing the original event-start receipt reference.

Publication transport is not installed in this slice. Its host ancestry helper
verifies the retained consumer journal, input manifest and original receipt
membership before deriving a `RootGroupID` and digest of the complete root set.
The store checks the same gaggle and exact reserved source RunID, derives depth
from all original members, and refuses root sets larger than 32 or depth above 8.
The authenticated publication service must also verify the active stage attempt
and allowed event types; the store helper itself does not authenticate a caller.
Child and human-continuation publication remain unsupported until their ancestry
adapters can provide equivalent evidence.

Acceptance copies every represented root into receipt-owned rows and marks each
workflow producer as a durable root dependency. The root-set digest detects
changed membership. A descendant still spends the original root allowances after
its parent's history is pruned or the queue reopens. A known workflow cannot
replace its recorded roots, and an event consumer cannot use its new RunID to
start a fresh root. Consumer admission checks all represented allowances before
charging any, so one exhausted root cannot partially spend another. Intake
reserves indexed root membership and per-consumer charges within the shared byte
ceiling. Retry recovers the same receipt and never charges roots again.
