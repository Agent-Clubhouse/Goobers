-- creates: control.acceptance
-- source: branch p3/start-fence migrations/0003_acceptance.expand.sql (#7840); delete this file when the migration merges

CREATE TABLE control.acceptance (
    instance_id text NOT NULL,
    id text NOT NULL,
    idem_key text NOT NULL,
    actor text NOT NULL,
    fingerprint bytea NOT NULL,
    payload bytea NOT NULL,
    state text NOT NULL CHECK (state IN ('accepted', 'dispatching', 'dispatched', 'rejected')),
    owner_epoch bigint,
    run_id text NOT NULL DEFAULT '',
    reason text NOT NULL DEFAULT '',
    accepted_at timestamptz NOT NULL,
    finished_at timestamptz,
    PRIMARY KEY (instance_id, id),
    UNIQUE (instance_id, idem_key)
);

CREATE INDEX acceptance_pending_idx
    ON control.acceptance (instance_id, accepted_at, id)
    WHERE state='accepted';

CREATE UNIQUE INDEX acceptance_run_idx
    ON control.acceptance (instance_id, run_id)
    WHERE run_id<>'';

CREATE TABLE control.source_receipt (
    instance_id text NOT NULL,
    source_key text NOT NULL,
    actor text NOT NULL,
    fingerprint text NOT NULL,
    acceptance_ids text[] NOT NULL,
    accepted_at timestamptz NOT NULL,
    PRIMARY KEY (instance_id, source_key)
);

CREATE TABLE control.source_cursor (
    instance_id text NOT NULL,
    scope text NOT NULL,
    revision text NOT NULL,
    cursor_at timestamptz NOT NULL,
    legacy_pending boolean NOT NULL DEFAULT false,
    occurrence_cursors timestamptz[],
    PRIMARY KEY (instance_id, scope)
);

CREATE TABLE control.schedule_demand (
    instance_id text NOT NULL,
    id text NOT NULL,
    scope text NOT NULL,
    payload bytea NOT NULL,
    observed_count integer NOT NULL DEFAULT -1,
    queued integer NOT NULL DEFAULT 0,
    accepted_at timestamptz NOT NULL,
    independent boolean NOT NULL DEFAULT false,
    fingerprint text NOT NULL,
    PRIMARY KEY (instance_id, id)
);

CREATE UNIQUE INDEX schedule_demand_coalesced_idx
    ON control.schedule_demand (instance_id, scope)
    WHERE independent=false;
