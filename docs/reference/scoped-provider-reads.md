# Scoped provider reads

This increment of HAW-EVT-008 connects the existing HTTP read cache to explicit
runtime scope. It does not introduce a second provider model or a new database.

## Isolation and callers

Each cache entry includes gaggle, automation binding, retained configuration
generation, provider kind, exact URL/query and complete request headers in its
hashed identity. Credential values are hashed, never stored as key text. Changes
to credentials, source generation or response representation cannot reuse an
older partition. Two gaggles do not share a response or an in-memory open-PR
refresher even if they use the same repository and credentials.

The daemon supplies its retained generation to backlog, refill, remediation and
open-PR readers. Ordinary deterministic CLI stages receive the automation binding
and generation only from an explicitly opted-in executor. Authored run variables
and ambient passthrough cannot choose those fields. Missing or unknown scope
bypasses caching. Generic/human executor construction does not opt into automation.
Cache scope does not grant permission: provider construction and credential
resolution remain in their existing owners.

## Reads and invalidation

Compatible GitHub list reads share the same scheduler evaluation snapshot.
Strong ETags and the existing Last-Modified fallback support conditional refresh;
weak ETags never validate filtered collection membership. ADO GET list reads
preserve continuation headers and use the same scoped store. ADO conditional
network reads retain reported quota/delay and remain distinct from snapshot
hits that make no provider request. Existing transports,
credential rotation, request quotas and retries remain in place.

Explicit fresh/conditional/range requests bypass scoped reuse. POSTs and writes
always use the normal transport in this increment. Only complete successful
bodies up to the cache's 16 MiB ceiling are buffered for storage. Larger responses
remain streaming and retain their original closer. `no-store` and `Vary: *`
responses do not enter the cache. Existing 512-entry and storage limits remain.

Snapshot invalidation affects only its gaggle/binding/generation partition. A
reused wrapper checks the shared store before replay, so its in-memory copy cannot
hide completed invalidation. The later [snapshot generation slice](provider-read-invalidation.md) also fences
late fills from provider requests still in flight when invalidation happened.

## Remaining work

Declared ADO WIQL/batch read plans, provider cost and scope metrics, foreground
freshness, background reconciliation, event invalidation, provider-wide write fences,
and the portal's separately authorized interactive binding are subsequent slices.
This increment does not claim to centralize every read or complete the shared
provider-read workstream.
