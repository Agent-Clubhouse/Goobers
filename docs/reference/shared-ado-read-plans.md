# Shared ADO list read plans

This slice extends [scoped provider reads](scoped-provider-reads.md) to ADO's
read-only POST endpoints used by `ListWorkItems`: WIQL selection and exact batch
hydration. It does not enable caching for arbitrary POST requests.

## Eligibility and isolation

The provider privately declares a typed list query or batch request. The cache
verifies the exact endpoint and a bounded digest of its serialized body. Request
headers, caller JSON and a matching URL cannot declare a read plan. Mutation
preflights and marker/idempotency searches keep their existing live transport.

Gaggle, binding, retained generation, provider, credentials, query parameters,
representation headers and complete query/batch bytes participate in isolation.
Narrowed WIQL and its fallback have different identities. A rejected narrowing
is not cached; successful fallback still applies the existing client-side field
predicate and reports fallback in page information. Batch hydration preserves
requested membership, ordering, omitted items and cursor accounting.

## Sharing and cancellation

Within an explicit scheduler evaluation snapshot, independent wrappers/processes
can share the stored response. Without an evaluation ID, only overlapping reads
in one process share a result; a later foreground read goes back to ADO. The
process nonce and monotonic completion sequence avoid wall-clock freshness
assumptions. A completed explicit snapshot invalidation is visible to reused
wrappers.

At most 64 reads wait for sharing, and lock acquisition remains bounded. Excess
callers use the ordinary provider transport. No background provider request is
created. Cancelling a waiter does not cancel its leader; cancelling the leader
stops its request and prevents caching its result. A live waiter may then perform
its own request. Provider request gates, retry behavior and rate-limit feedback
remain in their existing transport owners.

Only complete HTTP 200 responses within the existing response limit are eligible.
The provider validates their actual wire types, including individual members;
malformed/error responses are not stored. ADO null batch members remain valid
under the existing Omit policy. Explicit fresh requests and missing scope bypass
sharing. Cache hits are reported separately from network quota observations.

## Delivery boundary

Ordinary ADO stage construction already installs the scoped wrapper, so provider
list calls use this path without a second provider implementation. This does not
add portal credentials or authorization, generalize the daemon's supported
backlog-counter providers, or claim production ADO performance qualification.

Metrics, policy-driven foreground/background freshness, event invalidation and
in-flight write fences remain later work. Fewer HTTP calls do not establish a
proportional reduction in ADO resource cost.
