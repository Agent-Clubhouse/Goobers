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
lock protects sequence allocation and emit-key dedup across partitions. #7810
adds monthly partition management, health streams and consumer cursors without
replacing the parent table or changing cursor identity.

The full unchanged shared conformance suites and focused fencing/rollback tests
run with `cloudintegration` against a local PostgreSQL 16 container, using
`GOOBERS_TEST_POSTGRES_URL`. Each fixture creates its own database. The existing
cloud CI job runs this suite against its PostgreSQL 16 service. No Azure resources
or live model calls are needed.
