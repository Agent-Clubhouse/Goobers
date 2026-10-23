-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

CREATE TABLE control.provider_observation (
    instance_id text NOT NULL,
    observation_key text NOT NULL,
    payload jsonb NOT NULL,
    observed_at timestamptz NOT NULL,
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0),
    PRIMARY KEY (instance_id, observation_key),
    CHECK (length(observation_key) BETWEEN 1 AND 128),
    CHECK (octet_length(payload::text) <= 16384)
);
