-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- Retry custody is registered before a remote CAS and survives release,
-- workflow changes and run retention. No credentials are persisted here.
CREATE TABLE control.shared_claim_repository (
    instance_id text NOT NULL,
    repo_key text NOT NULL,
    repository jsonb NOT NULL,
    cursor text NOT NULL DEFAULT '',
    owner_epoch bigint NOT NULL,
    registered_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (instance_id, repo_key)
);
