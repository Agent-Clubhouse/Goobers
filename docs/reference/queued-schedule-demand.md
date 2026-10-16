# Durable demand-sized schedules

A scheduled workflow with a demand counter first records its due fire and exact
workflow configuration in the host queue. Only then does it query the captured
counter. A successful observation seals the eligible count; the scheduler moves
consecutive worker ordinals into ordinary start receipts as capacity becomes
available. Counts size workers, not item assignments or claims.

## Recovery and admission

The fire, its configuration pins and schedule cursor are committed together.
By default, new discovery coalesces occurrences from the preceding hour and records that
window and occurrence count. Expired discoveries do not query the provider.
This window never expires an already accepted or partially transferred obligation.
An interrupted or failed observation leaves an unsized obligation for a later
poll. Once observed, restart and configuration reload preserve the original
count without another provider read. A no-work observation closes the fire.
Existing timeout fallback behavior can seal one worker.

With the default policy, later due firings coalesce into an outstanding obligation without replacing its
configuration or count. Workers transfer in batches of at most 32, bounded by
current workflow capacity and existing queued occupancy. The ledger checks both
the ordinal interval and occupancy in the acceptance transaction. Concurrent
writers cannot transfer the same ordinal twice. Failed acceptance cannot launch
a worker directly. Capacity release wakes remaining demand.

Unobserved and partially transferred obligations retain their configuration
archives. Accepted workers then retain their own pins and use the ordinary
queued dispatcher, current eligibility checks and matching journal evidence.
Changing its schedule revision resets only the future evaluation cursor; an
older partially transferred obligation remains discoverable and completes using
its original count and pins. Removing or disabling a workflow does not silently delete an outstanding
obligation. Generic queue intervention and retirement controls remain separate
work; such obligations retain their archives until explicitly resolved.

## Storage and limits

An additive `schedule_demands` table lives in the existing host queue database.
Existing signal receipts and schedule cursors survive migration. Each obligation
shares the queue's 10,000-record and byte limits. Observed demand is bounded at
10,000 workers. Source receipts and completed starts retain seven days of replay
history, so history pressure can pause admission even when execution keeps up.
Capacity and retention tuning remain separate queue operations work.

Downgrading must preserve the database and configuration archives and restore a
compatible dispatcher; an older binary does not own these new obligations.
This adapter adds neither human authority nor cross-gaggle event delivery.

## Validation and relationship to other sources

A real daemon/runner journey captures two workers, transfers one, tears down the
host, changes the workflow, then reopens and completes both original workers
without recounting. Only provider sizing is substituted in that journey; it does
not qualify a live GitHub, ADO or Kubernetes deployment. Storage and scheduler
race tests cover concurrent transfer, restart, failed sizing, no-work, timeout
fallback, capacity release, archive retention and migration.

This LAND-E01 slice extends [plain scheduled starts](queued-scheduled-starts.md)
and [counted workers](queued-counted-workers.md). It narrowly adapts the scheduled
demand parts of reference snapshot
`fa34a754148ea3076bfd04fe4976a5a461b063f2`. Generic emitted events, configurable
debounce, shared provider polling and interactive restarts are separate slices.

## Independent occurrence custody foundation

The host queue can retain separate pinned schedule occurrences in one workflow
scope alongside an older coalesced obligation. The oldest outstanding occurrence
is selected for sizing; worker transfer resolves the selected occurrence by its
exact ID and verifies its gaggle/workflow scope. Each occurrence keeps its own
sealed count and next worker ordinal, and archive retention enumerates them all.

An additive queue migration preserves existing coalesced records, payloads,
acceptance times and partial worker progress. A partial unique index continues
to allow only one coalesced obligation per scope. Independent captures advance
their source cursor atomically; capacity or cursor conflicts roll back both
custody and progress. Exact retained-capture retries preserve original pins;
changing pins, scope, revision, cursor boundary or capture mode conflicts.
Completed occurrences cannot be recreated behind the advanced cursor. Older
binaries refuse the newer queue schema rather than silently dropping custody.

### Independent discovery

With `spec.scheduleCatchUp: all`, the runtime retains a separate nominal cursor for each
schedule declaration (maximum 1,000 declarations). One sweep captures at most 100
occurrences, choosing the earliest nominal time and declaration order for ties.
Each capture and position advance commit together; a failure leaves the next
occurrence pending. Interval declarations keep their own phase, including across
restart at a sweep boundary. This does not promise execution ordering.

Occurrence envelopes carry the one-based declaration index and exact previous and
next nominal times. Dispatch verifies them against the archived definition.
Existing coalesced pending custody transfers before independent positions are
initialized. Schedule revision changes reset future positions without changing
already accepted starts or sizing obligations. Older binaries refuse the additive
queue schema. Default capture fingerprints retain their prior representation.

Set `spec.scheduleCatchUp: all` to retain every missed nominal occurrence.
Omission or `coalesce` preserves one-hour coalescing. Both policies continue to
apply current admission limits; `all` can accumulate queued work during long
outages. Discovery never captures future occurrences, and queue pressure leaves
remaining occurrences pending at their durable positions.

Changing policy resets future discovery at the time the edit is observed. It
does not expand an old coalesced acceptance or replace already captured pins.
Previously accepted sizing obligations retain their archived counter even when
current authoring returns to ordinary scheduling. Sealed counts are not polled
again.

Independent occurrences acquire custody even during idle backoff. Their later
admission matches the archived declaration to the current declaration identity,
so reordering cannot apply a different schedule's backoff. A held occurrence keeps
its original pins and reserved run ID and retries normal admission after backoff.

While a configured workflow is paused, `all` still captures due occurrences but
does not start them or poll sizing providers. Resumption uses current enablement
and readiness with the original execution pins. Removing the workflow retains
its outstanding custody for explicit intervention rather than deleting it.

Accepted demand remains discoverable when current authoring removes its sizing
policy or every enabled schedule. Unsized demand uses its archived counter;
a sealed count is not polled again. Current admission still applies, and removing
the workflow itself does not create a replacement target. Schedule removal changes
only future discovery; accepted demand and worker payloads keep their original pins.
