# Gaggle event receipts and durable routing

Status: internal store implementation for HAW-EVT-001/003/007 and the HAW-EVT-006
root budget. Public ingress, workflow publication, host routing sweeps, pinned
consumer execution and normalization of every start source remain implementation
work. These store APIs do not enable an event endpoint or execute a consumer.

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
`goobers.event-start/v1` request requires a dedicated host launcher; the existing
generic dispatcher refuses it rather than executing a current-catalog alias.

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
bounded inventories before pruning those resources. Connecting these inventories
to journal/configuration-generation pruning remains host integration work. Without
consumer settlement, accepted inputs and starts remain retained.
