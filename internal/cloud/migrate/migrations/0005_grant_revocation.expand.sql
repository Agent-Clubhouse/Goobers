-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

CREATE TABLE control.grant_revocation (
    instance_id text NOT NULL,
    grant_id text NOT NULL,
    owner_epoch bigint NOT NULL,
    revoked_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (instance_id, grant_id)
);
