-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

CREATE SCHEMA IF NOT EXISTS journal;
CREATE TABLE journal.run (
    instance_id text NOT NULL,
    run_id text NOT NULL,
    gaggle text NOT NULL,
    workflow text NOT NULL,
    run_yaml bytea NOT NULL,
    created_at timestamptz NOT NULL,
    retired_at timestamptz,
    PRIMARY KEY (instance_id, run_id)
);
CREATE TABLE journal.run_seq (
    instance_id text NOT NULL,
    run_id text NOT NULL,
    last_seq bigint NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    owner_epoch bigint NOT NULL,
    owner_token text,
    last_append_at timestamptz,
    PRIMARY KEY (instance_id, run_id),
    FOREIGN KEY (instance_id, run_id) REFERENCES journal.run
) WITH (fillfactor=70);
CREATE TABLE journal.run_checkpoint (
    instance_id text NOT NULL,
    run_id text NOT NULL,
    phase text NOT NULL,
    terminal boolean NOT NULL,
    state bytea NOT NULL,
    at_seq bigint NOT NULL,
    PRIMARY KEY (instance_id, run_id),
    FOREIGN KEY (instance_id, run_id) REFERENCES journal.run
);
CREATE TABLE journal.run_event (
    instance_id text NOT NULL,
    run_id text NOT NULL,
    seq bigint NOT NULL CHECK (seq > 0),
    branch integer NOT NULL DEFAULT 0,
    type text NOT NULL,
    stage text NOT NULL DEFAULT '',
    attempt integer NOT NULL DEFAULT 0,
    emit_key text,
    at timestamptz NOT NULL,
    body bytea NOT NULL,
    PRIMARY KEY (instance_id, run_id, seq),
    FOREIGN KEY (instance_id, run_id) REFERENCES journal.run
);
CREATE UNIQUE INDEX run_event_emit_key ON journal.run_event(instance_id, run_id, emit_key) WHERE emit_key IS NOT NULL;
CREATE TABLE journal.blob (
    instance_id text NOT NULL,
    run_id text NOT NULL,
    path text NOT NULL,
    digest text NOT NULL,
    body bytea NOT NULL,
    PRIMARY KEY (instance_id, run_id, path),
    FOREIGN KEY (instance_id, run_id) REFERENCES journal.run
);
CREATE TABLE journal.run_artifact (
    instance_id text NOT NULL,
    run_id text NOT NULL,
    seq bigint NOT NULL,
    digest text NOT NULL,
    kind text NOT NULL,
    PRIMARY KEY (instance_id, run_id, seq, digest),
    FOREIGN KEY (instance_id, run_id, seq) REFERENCES journal.run_event
);
CREATE INDEX run_artifact_digest ON journal.run_artifact(digest);
-- Stream is part of every instance-log key from the start. HA-P3-10 can add
-- health streams, consumer cursors and partitioned event storage without
-- changing the public cursor identity (instance_id, stream, seq).
CREATE TABLE journal.instance_seq (
    instance_id text NOT NULL,
    stream text NOT NULL,
    last_seq bigint NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    owner_epoch bigint NOT NULL,
    owner_token text,
    PRIMARY KEY (instance_id, stream)
) WITH (fillfactor=70);
CREATE TABLE journal.instance_event (
    instance_id text NOT NULL,
    stream text NOT NULL,
    seq bigint NOT NULL CHECK (seq > 0),
    at timestamptz NOT NULL,
    type text NOT NULL,
    run_id text NOT NULL DEFAULT '',
    emit_key text,
    epoch bigint NOT NULL,
    body bytea NOT NULL,
    PRIMARY KEY (instance_id, stream, seq, at),
    FOREIGN KEY (instance_id, stream) REFERENCES journal.instance_seq
) PARTITION BY RANGE (at);
CREATE TABLE journal.instance_event_default PARTITION OF journal.instance_event DEFAULT;
-- Every writer holds instance_seq FOR UPDATE during dedup and allocation, so
-- uniqueness across time partitions does not depend on a partition-local index.
CREATE INDEX instance_event_emit_key ON journal.instance_event(instance_id, stream, emit_key) WHERE emit_key IS NOT NULL;
