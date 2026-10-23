-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

CREATE TABLE control.child_parent (
    instance_id text NOT NULL,
    gaggle text NOT NULL,
    parent_run_id text NOT NULL,
    cancelled_at timestamptz,
    settled_at timestamptz,
    cancel_actor text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    owner_epoch bigint NOT NULL,
    PRIMARY KEY (instance_id, gaggle, parent_run_id)
);

CREATE TABLE control.child_occurrence (
    instance_id text NOT NULL,
    gaggle text NOT NULL,
    parent_run_id text NOT NULL,
    occurrence text NOT NULL,
    accepted_count integer NOT NULL CHECK (accepted_count BETWEEN 0 AND 32),
    max_children integer NOT NULL CHECK (max_children BETWEEN 1 AND 32),
    owner_epoch bigint NOT NULL,
    PRIMARY KEY (instance_id, gaggle, parent_run_id, occurrence),
    FOREIGN KEY (instance_id, gaggle, parent_run_id)
        REFERENCES control.child_parent (instance_id, gaggle, parent_run_id)
);

CREATE TABLE control.child_lineage (
    instance_id text NOT NULL,
    gaggle text NOT NULL,
    parent_run_id text NOT NULL,
    occurrence text NOT NULL,
    invocation_key text NOT NULL,
    sequence integer NOT NULL,
    child_id text NOT NULL,
    acceptance_id text NOT NULL,
    start_key text NOT NULL,
    actor_digest text NOT NULL,
    payload_digest text NOT NULL,
    proposal_digest text NOT NULL DEFAULT '',
    snapshot_receipt_digest text NOT NULL DEFAULT '',
    snapshot_bundle_digest text NOT NULL DEFAULT '',
    result_receipt_digest text NOT NULL DEFAULT '',
    result_bundle_digest text NOT NULL DEFAULT '',
    state text NOT NULL CHECK (state IN (
        'queued','running','awaiting_human','completed','failed','cancelled'
    )),
    result_ref text NOT NULL DEFAULT '',
    workspace_ref text NOT NULL DEFAULT '',
    accepted_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    terminal_at timestamptz,
    acknowledged_at timestamptz,
    tombstoned_at timestamptz,
    owner_epoch bigint NOT NULL,
    PRIMARY KEY (instance_id, gaggle, parent_run_id, occurrence, invocation_key),
    UNIQUE (instance_id, child_id),
    UNIQUE (instance_id, acceptance_id),
    UNIQUE (instance_id, start_key),
    FOREIGN KEY (instance_id, gaggle, parent_run_id, occurrence)
        REFERENCES control.child_occurrence (instance_id, gaggle, parent_run_id, occurrence)
);

CREATE UNIQUE INDEX child_one_unresolved_idx
    ON control.child_lineage (instance_id, gaggle, parent_run_id, occurrence)
    WHERE acknowledged_at IS NULL;
CREATE INDEX child_active_idx
    ON control.child_lineage (instance_id, child_id)
    WHERE tombstoned_at IS NULL;
CREATE INDEX child_retention_idx
    ON control.child_lineage (instance_id, tombstoned_at, acknowledged_at, child_id);

CREATE TABLE control.child_authority (
    instance_id text NOT NULL,
    gaggle text NOT NULL,
    parent_run_id text NOT NULL,
    occurrence text NOT NULL,
    grant_id text NOT NULL,
    attempt_id text NOT NULL,
    config_digest text NOT NULL,
    policy_digest text NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked boolean NOT NULL DEFAULT false,
    owner_epoch bigint NOT NULL,
    PRIMARY KEY (instance_id, gaggle, parent_run_id, occurrence)
);

CREATE TABLE control.child_blob (
    instance_id text NOT NULL,
    child_id text NOT NULL,
    digest text NOT NULL,
    kind text NOT NULL CHECK (kind IN (
        'proposal','carrier','snapshot-receipt','snapshot-bundle',
        'result-receipt','result-bundle','publication-intent','publication-receipt',
        'disposition-plan'
    )),
    size_bytes bigint NOT NULL CHECK (size_bytes >= 0),
    owner_epoch bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (instance_id, child_id, kind, digest),
    FOREIGN KEY (instance_id, child_id)
        REFERENCES control.child_lineage (instance_id, child_id) ON DELETE CASCADE
);

CREATE TABLE control.child_blob_read (
    instance_id text NOT NULL,
    child_id text NOT NULL,
    contract_digest text NOT NULL,
    digest text NOT NULL,
    owner_epoch bigint NOT NULL,
    PRIMARY KEY (instance_id, child_id, contract_digest, digest),
    FOREIGN KEY (instance_id, child_id)
        REFERENCES control.child_lineage (instance_id, child_id) ON DELETE CASCADE
);

CREATE TABLE control.child_publication (
    instance_id text NOT NULL,
    child_id text NOT NULL,
    action text NOT NULL CHECK (action IN ('branch','pr')),
    execution_run_id text NOT NULL DEFAULT '',
    intent_digest text NOT NULL,
    state text NOT NULL CHECK (state IN ('prepared','effect_pending','confirmed')),
    receipt_digest text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    owner_epoch bigint NOT NULL,
    PRIMARY KEY (instance_id, child_id, action),
    FOREIGN KEY (instance_id, child_id)
        REFERENCES control.child_lineage (instance_id, child_id) ON DELETE CASCADE
);

CREATE TABLE control.child_disposition (
    instance_id text NOT NULL,
    child_id text NOT NULL,
    request_digest text NOT NULL,
    previous_digest text NOT NULL DEFAULT '',
    action text NOT NULL CHECK (action IN ('merge','replace','discard')),
    result_ref text NOT NULL,
    attempt_id text NOT NULL,
    requested_at timestamptz NOT NULL,
    plan_digest text NOT NULL DEFAULT '',
    applied_at timestamptz,
    owner_epoch bigint NOT NULL,
    PRIMARY KEY (instance_id, child_id),
    FOREIGN KEY (instance_id, child_id)
        REFERENCES control.child_lineage (instance_id, child_id) ON DELETE CASCADE
);

CREATE TABLE control.child_disposition_history (
    instance_id text NOT NULL,
    child_id text NOT NULL,
    revision integer NOT NULL,
    request_digest text NOT NULL,
    previous_digest text NOT NULL DEFAULT '',
    action text NOT NULL,
    result_ref text NOT NULL,
    attempt_id text NOT NULL,
    requested_at timestamptz NOT NULL,
    plan_digest text NOT NULL DEFAULT '',
    superseded_at timestamptz NOT NULL,
    owner_epoch bigint NOT NULL,
    PRIMARY KEY (instance_id, child_id, revision),
    FOREIGN KEY (instance_id, child_id)
        REFERENCES control.child_lineage (instance_id, child_id) ON DELETE CASCADE
);
