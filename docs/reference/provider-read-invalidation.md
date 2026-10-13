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

The explicit snapshot API above refreshes one evaluation partition. The provider
write hooks below extend it with a gaggle/provider mutation generation.

## Refresh after provider writes

Scoped provider clients additionally resolve a shared mutation generation for
one gaggle and provider before each eligible GET or declared ADO read plan. A
confirmed write rotates this generation. This fences both evaluation snapshots
and base conditional entries, including responses still in flight in another
process. Later reads cannot send validators from the prior generation. A caller
already in flight may still receive its response.

Mutation generations span credential bindings and retained configuration versions
within the same gaggle/provider: a write can change what either binding reads.
Response bodies remain separately partitioned by binding, configuration,
credentials, endpoint and representation. Other gaggles and providers keep their
snapshots. Invalidation is deliberately broader than a repository or item; this
slice does not add a dependency index.

The provider reports effects independently of its optional run-journal recorder.
Existing issue, PR, comment, review, claim, CI and repository effects use those
hooks; native GitHub parent/blocker links, repository-label creation, partial
label edits, ADO branch/commit operations, reviewer requests and work-item comment
and patch helpers also report successful effects. Merge and queue operations
retain their existing acknowledgment, intent and receipt requirements. A receipt
failure after an acknowledged write still refreshes reads without changing the
reported write/receipt outcome. Rejected operations do not report a successful
effect; confirmed absence after label removal can also refresh a stale list.

This is based on provider operation semantics, not HTTP verbs. ADO WIQL, batch
hydration and permission queries use POST without invalidating reads; GitHub
GraphQL queries likewise do not imply a write. Unscoped transports and writes
outside these provider methods do not acquire a gaggle identity automatically.

Invalidation uses the same bounded disposable generation store and introduces no
new database schema. It stores neither write bodies nor credentials. If saving
invalidation fails, the writing wrapper permanently bypasses shared reuse and the
successful provider result is preserved. Other processes may retain their
existing evaluation snapshots until normal refresh or expiry in this failure
case. There is no atomic transaction with GitHub/ADO, and an ambiguous external
write whose success cannot be confirmed has no global freshness guarantee.
External edits, generic events, interactive permission checks and live provider
performance qualification remain separate work.
