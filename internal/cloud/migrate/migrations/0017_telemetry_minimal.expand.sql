-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

CREATE SCHEMA IF NOT EXISTS telemetry;

CREATE TABLE telemetry.first_success_milestones (
    instance_id text PRIMARY KEY,
    init_completed_at timestamptz,
    first_pr_open_at timestamptz,
    owner_epoch bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE telemetry.defect_aggregate (
    instance_id text NOT NULL,
    gaggle text NOT NULL,
    workflow text NOT NULL DEFAULT '',
    aggregate text NOT NULL CHECK (aggregate IN (
        'stage-failure-rate','gate-noise','credit-assignment','error-signature'
    )),
    kind text NOT NULL,
    subject text NOT NULL,
    error_class text NOT NULL DEFAULT '',
    signature text NOT NULL DEFAULT '',
    observed_at timestamptz NOT NULL,
    finding jsonb NOT NULL,
    source_stream text NOT NULL,
    source_seq bigint NOT NULL CHECK (source_seq > 0),
    owner_epoch bigint NOT NULL,
    -- One row per (aggregate identity, source stream): stream sequences have no
    -- shared order, so each source keeps its own contribution and watermark and
    -- the reader merges them (latest observed_at wins; ties by stream name).
    PRIMARY KEY (
        instance_id,gaggle,workflow,aggregate,kind,subject,error_class,signature,
        source_stream
    )
);

CREATE INDEX defect_aggregate_window_idx
    ON telemetry.defect_aggregate (
        instance_id,gaggle,workflow,aggregate,observed_at
    );

-- Gate noise is derived at read time from COMPLETE per-source gate totals (the
-- local rollup derives both gate kinds from one set of counts, so a finding from
-- one source cannot recover another kind's denominator). One row per
-- (gate, source stream); the read adds the sources and applies the thresholds.
CREATE TABLE telemetry.gate_total (
    instance_id text NOT NULL,
    gaggle text NOT NULL,
    workflow text NOT NULL DEFAULT '',
    gate text NOT NULL,
    source_stream text NOT NULL,
    total bigint NOT NULL CHECK (total >= 0),
    non_pass bigint NOT NULL CHECK (non_pass >= 0),
    escalated bigint NOT NULL CHECK (escalated >= 0),
    evaluated_runs jsonb NOT NULL DEFAULT '[]',
    escalated_runs jsonb NOT NULL DEFAULT '[]',
    observed_at timestamptz NOT NULL,
    source_seq bigint NOT NULL CHECK (source_seq > 0),
    owner_epoch bigint NOT NULL,
    PRIMARY KEY (instance_id,gaggle,workflow,gate,source_stream),
    CHECK (non_pass <= total AND escalated <= total)
);

CREATE TABLE telemetry.consumer_cursor (
    instance_id text NOT NULL,
    consumer text NOT NULL,
    stream text NOT NULL,
    seq bigint NOT NULL DEFAULT 0 CHECK (seq >= 0),
    fingerprint text NOT NULL DEFAULT '',
    owner_epoch bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (instance_id,consumer,stream)
);
