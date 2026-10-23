-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- One immutable outcome per run/branch/stage/occurrence/attempt. The controller
-- stamps its live lease epoch; worker pods only use the observation plane.
CREATE TABLE control.runner_bandit (
    instance_id text NOT NULL,
    record_key text NOT NULL,
    stage text NOT NULL,
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (instance_id, record_key)
);

CREATE INDEX runner_bandit_stage_idx
    ON control.runner_bandit (instance_id, stage, record_key);
