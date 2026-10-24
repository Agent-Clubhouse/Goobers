-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

CREATE TABLE journal.health_event (
    instance_id text NOT NULL,
    stream text NOT NULL,
    seq bigint NOT NULL CHECK (seq > 0),
    at timestamptz NOT NULL,
    type text NOT NULL,
    gaggle text NOT NULL DEFAULT '',
    emit_key text,
    epoch bigint NOT NULL,
    body bytea NOT NULL,
    PRIMARY KEY (instance_id, stream, seq, at),
    FOREIGN KEY (instance_id, stream) REFERENCES journal.instance_seq
) PARTITION BY RANGE (at);

CREATE TABLE journal.health_event_default
    PARTITION OF journal.health_event DEFAULT;

-- Health dedup stays partition-local: the emit key hashes the event including
-- its OccurredAt, which also selects the month, so a retry always lands in the
-- same partition. health_event is created here, so this index is built on an
-- empty table.
CREATE UNIQUE INDEX health_event_default_emit_key
    ON journal.health_event_default (instance_id, stream, emit_key)
    WHERE emit_key IS NOT NULL;

-- Scheduler emit keys must dedup independent of the month the retry's append
-- clock falls in, and unique indexes cannot span partitions. An unpartitioned
-- table, written in the same transaction as the event under the instance_seq
-- row lock, is the cross-month guard. Events #7853 already wrote are backfilled
-- (first sequence wins; the rows are scheduler bookkeeping, so the scan is small).
CREATE TABLE journal.instance_emit_key (
    instance_id text NOT NULL,
    stream text NOT NULL,
    emit_key text NOT NULL,
    seq bigint NOT NULL CHECK (seq > 0),
    PRIMARY KEY (instance_id, stream, emit_key),
    FOREIGN KEY (instance_id, stream) REFERENCES journal.instance_seq
);
-- The previous binary (Recreate rollout: N-1 serves until it is stopped) keeps
-- appending scheduler events with emit keys and knows nothing of this table. A
-- row trigger keeps the key table complete for every writer, so a successor's
-- retry of an N-1 append dedups. Created BEFORE the backfill: the trigger and
-- the backfill together cover every row, with ON CONFLICT making overlap harmless.
-- Row triggers on a partitioned table are cloned onto every partition,
-- including ones ATTACHed later.
CREATE FUNCTION journal.instance_event_record_emit_key() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    IF NEW.emit_key IS NOT NULL THEN
        INSERT INTO journal.instance_emit_key (instance_id, stream, emit_key, seq)
        VALUES (NEW.instance_id, NEW.stream, NEW.emit_key, NEW.seq)
        ON CONFLICT DO NOTHING;
    END IF;
    RETURN NULL;
END
$fn$;
CREATE TRIGGER instance_event_emit_key
    AFTER INSERT ON journal.instance_event
    FOR EACH ROW EXECUTE FUNCTION journal.instance_event_record_emit_key();
INSERT INTO journal.instance_emit_key (instance_id, stream, emit_key, seq)
SELECT DISTINCT ON (instance_id, stream, emit_key) instance_id, stream, emit_key, seq
FROM journal.instance_event
WHERE emit_key IS NOT NULL
ORDER BY instance_id, stream, emit_key, seq
ON CONFLICT DO NOTHING;

-- Monthly partitions are NOT created here. A live #7853 install keeps this
-- month's rows in instance_event_default, and attaching a partition over rows
-- DEFAULT holds fails (23514). The migrate runner's fenced partition
-- maintenance (migrate.EnsureMonthlyPartitions) creates the current and future
-- months, moving any DEFAULT rows into the new partition before attaching it.

CREATE TABLE journal.consumer_cursor (
    instance_id text NOT NULL,
    consumer text NOT NULL,
    stream text NOT NULL,
    seq bigint NOT NULL DEFAULT 0 CHECK (seq >= 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (instance_id, consumer, stream),
    FOREIGN KEY (instance_id, stream) REFERENCES journal.instance_seq
);
