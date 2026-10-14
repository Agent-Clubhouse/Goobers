# Provider snapshot invalidation

This slice extends [scoped provider reads](scoped-provider-reads.md) and
[ADO list read plans](shared-ado-read-plans.md) with durable snapshot generations.
It addresses a race where a read starts, another process invalidates its snapshot,
and the first response arrives late enough to refill the invalidated snapshot.

Each snapshot has a random generation in the shared disposable response store.
A read captures that generation before accessing or filling a snapshot alias.
Invalidation atomically rotates the generation and removes the snapshot's current
aliases. Subsequent reads resolve the new generation, so a late fill under the
old generation is unreachable. The original in-flight caller can still receive
its response; invalidation does not cancel network work or promise a transaction
spanning the external provider and the local cache.

Both GET snapshots and declared ADO WIQL/batch snapshots use this path. Existing
base conditional entries remain available for provider validation; invalidation
does not turn a conditional base entry into a free snapshot hit. Gaggle, binding
and retained configuration scope continue to partition snapshot identities.
Unrelated snapshots keep their generation.

Generation metadata is bounded to the response-entry limit (512 in the runtime).
Evicting a generation is safe: a later read creates a new random identity, so it
cannot revive earlier responses. Old late-fill aliases remain subject to existing
cache count, byte and expiry bounds. A storage or generation-read failure falls
back to the normal provider request rather than reusing an unverified snapshot.

The disposable cache advances from schema 1 to schema 2 with one generation table
and index; existing response data is retained. Older binaries refuse this newer
cache schema and therefore use their ordinary live provider path. No authoritative
workflow or backlog data is migrated.

This provides fencing for explicit snapshot invalidation. It does not add new
write hooks, event-driven invalidation, a provider-wide freshness guarantee, or
interactive authorization. Those remain separate workstream requirements.
