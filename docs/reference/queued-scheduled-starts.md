# Durable scheduled starts

Plain schedule triggers enter the host's existing start queue before execution.
Each accepted firing retains its workflow definition and configuration generation.
The scheduler checks current eligibility and available capacity when dispatching
that definition. A queued receipt is not a running workflow.

## Timing and capacity

The scheduler stores an evaluation cursor for each gaggle and workflow, bound
to a revision of its normalized schedules and configured timezone. A schedule
edit starts future evaluation at the first observation of that revision. Other
workflow edits preserve the cursor. Previously accepted starts and partially
transferred demand keep their original definitions; a stale revision cannot
advance the new cursor. A due
observation commits its start and advances that cursor in one transaction. Missed occurrences in the preceding hour coalesce into one catch-up start.
The captured source records the original evaluation span, eligible window and
occurrence count. Older, newly discovered occurrences expire: an empty receipt
and cursor advance prevent their resurrection after restart. Accepted starts
and outstanding demand retain custody regardless of age. If storage or configuration capture fails, the cursor does
not advance and the due interval remains eligible for retry.

A later due observation can enqueue another start while an earlier start is
waiting for capacity. This replaces the old single pending-fire marker for plain
schedules: accepted observations remain distinct durable receipts. Queue limits
bound storage; capacity limits still govern execution. The current shared limit
is 10,000 records, including retained source receipts and completed starts.
Completed records normally retain seven days of replay history, so sustained
frequent schedules can fill the queue even when execution keeps up. Admission
then pauses with the cursor unchanged until records expire; the next accepted
observation coalesces only occurrences still within the one-hour window. This adapter does not
provide configurable debounce or drop previously accepted starts.

Adaptive idle backoff still applies. A suppressed observation commits an empty
source receipt and advances the cursor, so it is not replayed as work after a
restart. Successful dispatches keep the original schedule provenance and update
idle backoff from their actual no-work result.

## Restart and upgrade

The durable cursor is authoritative after initial adoption. Older evaluation
files, including a file left ahead of the cursor, cannot skip or repeat a
committed firing. An outstanding legacy pending fire transfers once into the
new queue, even before the next cron interval is due. Replaying the old auxiliary
file cannot resurrect it.

This slice adds the `source_start_cursors` table; schedule revision tracking
adds its `revision` column. Existing unversioned cursors and their pending
legacy fire are adopted once without resetting them. Cursors, source receipts and
starts share the existing queue count and byte bounds. Cursors are retained for
workflow identity continuity; removing a workflow does not erase its cursor.
Configuration archives stay retained while accepted starts remain unfinished.
The database refuses older schema writers. Preserve the queue and upgrade to a
compatible dispatcher that understands captured catch-up metadata when
outstanding starts exist; deleting custody is not a
rollback procedure.

## Scope

This LAND-E01 adapter covers plain cron/interval schedules, including schedules
used for polling fallback when they have no demand counter.
[Demand-sized schedules](queued-schedule-demand.md) and
[counted workers](queued-counted-workers.md) have separate adapters. It does not complete the requirement
that every workflow start use a durable queue. The default one-hour coalesced
catch-up window is implemented. The accepted design’s explicit all-occurrences
policy, bounded to 100 occurrences per sweep, still needs its own authoring and
implementation.

The implementation adapts the schedule cursor and pinned schedule-start pieces
of reference snapshot `fa34a754148ea3076bfd04fe4976a5a461b063f2` to the current
scheduler. It extends [durable signal starts](queued-signal-starts.md) and uses
the same captured-definition dispatch as [ordinary HTTP starts](pinned-http-starts.md).
