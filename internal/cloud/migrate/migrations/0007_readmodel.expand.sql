-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

CREATE SCHEMA IF NOT EXISTS readmodel_v1;

CREATE TABLE IF NOT EXISTS readmodel_v1.meta (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    epoch text NOT NULL,
    min_change_seq bigint NOT NULL DEFAULT 0 CHECK (min_change_seq >= 0),
    next_change_seq bigint NOT NULL DEFAULT 0 CHECK (next_change_seq >= 0),
    built_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    ready boolean NOT NULL DEFAULT false,
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0)
);

CREATE TABLE IF NOT EXISTS readmodel_v1.run (
    run_id text PRIMARY KEY,
    source_seq bigint NOT NULL CHECK (source_seq >= 0),
    projection jsonb NOT NULL,
    gaggle text NOT NULL,
    workflow text NOT NULL,
    phase text NOT NULL,
    terminal boolean NOT NULL,
    started_at timestamptz NOT NULL,
    last_activity_at timestamptz NOT NULL,
    disposition text NOT NULL,
    stages text[] NOT NULL DEFAULT '{}',
    any_token_measured boolean NOT NULL DEFAULT false,
    any_premium_measured boolean NOT NULL DEFAULT false,
    any_cost_measured boolean NOT NULL DEFAULT false,
    any_retry_waste boolean NOT NULL DEFAULT false,
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0)
);

CREATE INDEX IF NOT EXISTS readmodel_v1_run_recency
    ON readmodel_v1.run (started_at DESC, run_id);
CREATE INDEX IF NOT EXISTS readmodel_v1_run_activity
    ON readmodel_v1.run (last_activity_at DESC, run_id);
CREATE INDEX IF NOT EXISTS readmodel_v1_run_gaggle_recency
    ON readmodel_v1.run (gaggle, started_at DESC, run_id);
CREATE INDEX IF NOT EXISTS readmodel_v1_run_gaggle_workflow_recency
    ON readmodel_v1.run (gaggle, workflow, started_at DESC, run_id);
CREATE INDEX IF NOT EXISTS readmodel_v1_run_phase_recency
    ON readmodel_v1.run (phase, started_at DESC, run_id);
CREATE INDEX IF NOT EXISTS readmodel_v1_run_gaggle_phase_recency
    ON readmodel_v1.run (gaggle, phase, started_at DESC, run_id);
CREATE INDEX IF NOT EXISTS readmodel_v1_run_gaggle_workflow_phase_recency
    ON readmodel_v1.run (gaggle, workflow, phase, started_at DESC, run_id);
CREATE INDEX IF NOT EXISTS readmodel_v1_run_stages
    ON readmodel_v1.run USING gin (stages);

CREATE TABLE IF NOT EXISTS readmodel_v1.change (
    seq bigint PRIMARY KEY CHECK (seq > 0),
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    kind text NOT NULL,
    run_id text NOT NULL,
    gaggle text NOT NULL,
    workflow text NOT NULL,
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0)
);

CREATE TABLE IF NOT EXISTS readmodel_v1.source_watermark (
    run_id text PRIMARY KEY,
    source_seq bigint NOT NULL CHECK (source_seq >= 0),
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0)
);

ALTER TABLE control.readmodel_version
    ADD COLUMN IF NOT EXISTS owner_epoch bigint NOT NULL DEFAULT 1;

INSERT INTO control.readmodel_version (name, active_version, owner_epoch)
VALUES ('readmodel', 1, 1)
ON CONFLICT (name) DO NOTHING;

INSERT INTO readmodel_v1.meta (epoch, ready, owner_epoch)
VALUES (gen_random_uuid()::text, true, 1)
ON CONFLICT (singleton) DO NOTHING;
