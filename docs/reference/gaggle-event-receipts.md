# Gaggle event receipt foundation

Status: implementation foundation for HAW-EVT-001 and HAW-EVT-007. Public ingress,
workflow publication, consumer routing/debounce and normalization of every start
source remain implementation work. This change does not enable an event endpoint.

The existing durable trigger database now accepts internal event receipts. Intake
commits the authenticated producer binding, gaggle, bounded event envelope and
matched routing snapshot together. Transport adapters must construct authority
from authenticated configuration, scrub secrets before intake, and enforce ingress
quotas and root-chain limits. Payload fields never select a gaggle or producer.

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
terminal receipt. Matched events retain `routing_pending` custody for the future
router. The store reserves ordinary start payload and maintenance space for up to
32 matched consumers before acknowledging intake. Existing manual/child intake
counts those reservations, so it cannot spend event completion headroom.

Routing that has not completed within one hour becomes `routing_failed` with a
reason. This is not a failed consumer run. At this implementation stage no router
creates deliveries or runs. Debounce definitions and already evaluated nonempty
keys are captured in each matched route; grouping is a subsequent implementation.

## Bounds and maintenance

Full receipts are limited to 10,000 per gaggle, tombstones to 100,000 per gaggle,
and all receipts share the existing instance database's 256 MiB hard ceiling and
20 percent maintenance headroom. Physical colocation does not grant cross-gaggle
read access. The initial physical byte ceiling is instance-wide, so one gaggle
can cause intake backpressure for another; per-gaggle fairness remains queue work.

Completed unmatched/failed receipts retain payloads and routes for seven days.
The daemon then retains identity/digest tombstones for another thirty days. Retry
during that window recovers the tombstone and cannot silently republish. After
tombstone removal, infinite deduplication is not promised. Full tombstone quotas
backpressure intake rather than deleting a deduplication promise early. Maintenance
is bounded to 100 records and a 50 ms context per daemon sweep, including when no
scheduler is attached. Future delivered receipts must retain their dependency
graph until the final consumer settles; this foundation only terminalizes receipts
without delivered consumer dependencies.
