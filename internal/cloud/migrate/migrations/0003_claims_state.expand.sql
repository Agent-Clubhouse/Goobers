-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

CREATE TABLE control.claim (
    instance_id text NOT NULL,
    gaggle text NOT NULL,
    provider text NOT NULL,
    external_id text NOT NULL,
    run_id text NOT NULL,
    workflow text NOT NULL,
    owner_token text NOT NULL,
    owner_epoch bigint NOT NULL,
    claimed_at timestamptz NOT NULL,
    renewed_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    verification jsonb,
    PRIMARY KEY (instance_id, gaggle, provider, external_id)
);

CREATE TABLE control.claim_history (
    instance_id text NOT NULL,
    gaggle text NOT NULL,
    provider text NOT NULL,
    external_id text NOT NULL,
    run_id text NOT NULL,
    workflow text NOT NULL,
    owner_token text NOT NULL,
    owner_epoch bigint NOT NULL,
    claimed_at timestamptz NOT NULL,
    renewed_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    released_at timestamptz NOT NULL,
    release_reason jsonb NOT NULL,
    verification jsonb,
    PRIMARY KEY (instance_id, gaggle, provider, external_id, owner_token)
);

CREATE INDEX claim_history_retention_idx
    ON control.claim_history (instance_id, released_at);

CREATE TABLE control.scheduler_state (
    instance_id text NOT NULL,
    scope text NOT NULL,
    key text NOT NULL,
    value bytea NOT NULL CHECK (octet_length(value) <= 8388608),
    etag bigint NOT NULL,
    owner_epoch bigint NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (instance_id, scope, key)
);
