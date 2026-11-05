# PostgreSQL journal backend

`New(Config{Pool, InstanceID, Lease, LiveOwner})` implements both
`journal.Backend` and `journal.ReaderBackend`. Role composition is tracked by
#7818. Read-only replicas need only the pool and instance ID; mutations fail
closed without the current controller lease. The cloud implementation imports
pgx; the shared journal and the `goobers` production closure do not.

Every mutation uses `lease.Manager.WithFence` and predicates SQL writes on the
live lease epoch. Open writers also carry random incarnation tokens persisted
with their counter rows. A newer controller epoch can recover abandoned writers;
a token from the previous incarnation cannot write even after its process learns
the new lease. Retiring a writer releases ownership without inventing a terminal
run event. Recovery returns exactly the committed event history.

Scrubbing and input preparation happen before the transaction. Under a row lock,
`UPDATE ... last_seq=last_seq+1 RETURNING last_seq` allocates sequences in the same
transaction that inserts the original JSON bytes and artifact indexes. No PG
sequence object is used. `AppendBatch` is all-or-nothing: rollback consumes no
sequence numbers. The `emitKey` runner field deduplicates retries. Clients must
reuse that key when retrying an uncertain commit; a two-second admission deadline
can refuse saturated writers before they acquire a connection or row lock.

History-dependent replay and arbitrary append predicates run before the fence.
Under the append lock, only events newer than that snapshot are read and
folded into the prepared state or predicate. Steady concurrent telemetry cannot
force a full replay retry or starve a gate/terminal append. Lifecycle/checkpoint replay remains
O(history) outside the write budget, while fenced work is O(batch). Progress-rate
checks use a bounded indexed window count; operator keys, notification claims and
child continuations use dedicated indexes. Recovery prepares its report before
fenced takeover and cleans transcript fragments in batches of at most 100.
The caller context bounds replay; the 30-second lock-wait budget applies only
to ownership contention, so it does not impose a second history-size ceiling.
A process-lifetime backend forgets local handles from strictly older epochs on
recovery, so lease reacquisition does not require restarting the process.

Run identity and input snapshots are immutable. Artifact/span bytes live in a
content-addressed PG blob table and are verified on reads. Transcript checkpoints
store only safe scrubbed prefixes. Final transcript publication commits before
private-partial deletion; recovery repeats interrupted cleanup. Operator-message
and notification claims resolve against durable history under the same lock.
Phase changes and explicit checkpoints update derived state; replay remains the
source of truth for branches, phase and recovery.

`RunNotifications` runs on a dedicated connection and coalesces wakeups every
250ms. It never runs in an append transaction. Read consumers must keep their
one-second polling fallback: a crash between commit and wakeup can lose a hint.
The caller owns the notifier connection and role-lifetime cancellation.

The minimal instance journal is the `scheduler` stream. It supports durable
open, append, retire, reopen and cursor reads. `BindInstance` is a local-file
compatibility API and is deliberately refused: arbitrary external appenders
must not bypass PG fencing. The instance event table starts as a range-partitioned
parent with one default partition; `(instance_id, stream, seq)` is the logical
cursor and the primary key also carries the partition timestamp. The counter
lock protects sequence allocation. Scheduler emit-key dedup uses the
unpartitioned `journal.instance_emit_key` table written in the same
transaction, so a retry is idempotent regardless of which month the append
clock falls in (unique indexes cannot span partitions). Health dedup stays
partition-local because its key hashes `OccurredAt`, which also picks the month.

A row trigger on `instance_event` also records every emit key, so events
written by the previous binary during a Recreate rollout are deduplicated too.

Monthly partitions are created by `migrate.PartitionMaintainer` (DB clock, UTC
months, current + 3 ahead, plus any past month still holding DEFAULT rows). Rows
already in DEFAULT move into a private table and the table is attached in ONE
transaction: until COMMIT every row is still visible through the parent, and any
failure (lock not obtainable, lost lease, timeout) rolls back with nothing hidden,
so `(stream, seq)` cursor consumers can never skip an event. Row movement uses
bounded keyset statements and only row locks, so appends keep flowing; the final
sweep + ATTACH hold the DEFAULT lock briefly with savepoint retries, and lease
ownership is confirmed (share lock) right before COMMIT. `migrate apply` attaches
only past and FUTURE months (the previous binary, with no retry for the ATTACH
routing race, is still serving and only the current month has live writers); the
current month is attached by the controller's periodic pass (#7882), which runs
only the new binary. Until then current-month rows simply stay in DEFAULT. `InstanceAppend` retries the one-instant partition-routing race that the ATTACH
of the current month can cause. Retention must drop a month with
`migrate.DropInstanceEventPartition`, which also deletes that month's
`instance_emit_key` rows in the same transaction. Health streams and consumer cursors extend the
same schema without replacing the parent table or changing cursor identity.

The full unchanged shared conformance suites and focused fencing/rollback tests
run with `cloudintegration` against a local PostgreSQL 16 container, using
`GOOBERS_TEST_POSTGRES_URL`. Each fixture creates its own database. The existing
cloud CI job runs this suite against its PostgreSQL 16 service. The HA nightly workflow publishes `BenchmarkPostgresJournalAppend` throughput
as a job summary and artifact; no throughput threshold gates merges. No Azure
resources or live model calls are needed.
