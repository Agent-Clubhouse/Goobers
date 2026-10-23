-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

CREATE TABLE control.stall_action (
    instance_id text NOT NULL,
    run_id text NOT NULL,
    identity jsonb NOT NULL,
    reason text NOT NULL CHECK (reason IN ('stalled','max_duration')),
    state text NOT NULL CHECK (state IN (
        'pending_cancel','cancelling','cancel_requested','terminating','terminated'
    )),
    detected_at timestamptz NOT NULL,
    cancel_requested_at timestamptz,
    terminated_at timestamptz,
    last_error text NOT NULL DEFAULT '',
    retry_at timestamptz,
    owner_epoch bigint NOT NULL,
    PRIMARY KEY (instance_id, run_id)
);

CREATE INDEX stall_action_pending_idx
    ON control.stall_action (instance_id, state, detected_at, run_id);
