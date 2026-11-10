-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- Learned dependency blocks (#552): the cloud counterpart of the daemon's
-- blocked.json. A record is keyed by its repository-scoped item, replaced
-- whole on a repeated block, and read together with its siblings to find the
-- dependency cycle a new block closes. record is the JSON the daemon writes.
CREATE TABLE control.blocked_record (
    instance_id text NOT NULL,
    record_key text NOT NULL CHECK (record_key <> ''),
    record jsonb NOT NULL,
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0),
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, record_key)
);
