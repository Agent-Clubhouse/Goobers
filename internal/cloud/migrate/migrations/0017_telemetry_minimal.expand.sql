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
    PRIMARY KEY (
        instance_id,gaggle,workflow,aggregate,kind,subject,error_class,signature
    )
);

CREATE INDEX defect_aggregate_window_idx
    ON telemetry.defect_aggregate (
        instance_id,gaggle,workflow,aggregate,observed_at
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
