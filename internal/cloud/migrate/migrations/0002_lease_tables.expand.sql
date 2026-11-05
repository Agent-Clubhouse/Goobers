-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

CREATE TABLE control.lease (
    instance_id text NOT NULL,
    name text NOT NULL,
    holder text NOT NULL,
    epoch bigint NOT NULL,
    PRIMARY KEY (instance_id, name)
);

CREATE TABLE control.lease_heartbeat (
    instance_id text NOT NULL,
    name text NOT NULL,
    holder text NOT NULL,
    epoch bigint NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (instance_id, name)
) WITH (fillfactor = 50);

CREATE TABLE control.lease_history (
    instance_id text NOT NULL,
    name text NOT NULL,
    epoch bigint NOT NULL,
    holder text NOT NULL,
    acquired_at timestamptz NOT NULL,
    released_at timestamptz,
    release_kind text CHECK (release_kind IN ('voluntary', 'expired')),
    PRIMARY KEY (instance_id, name, epoch)
);
