-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- A run's terminal frame completed under some leader. A successor skips these
-- runs when it re-lists recently closed runs, so a lease change does not
-- re-fire every hook for every run closed in the discovery window. It is in
-- ADDITION to per-hook idempotency: a frame that crashed before this row was
-- written is re-fired, and its hooks must tolerate that. Rows are pruned after
-- the discovery window has long passed.
CREATE TABLE control.terminal_framed (
    instance_id text NOT NULL,
    run_id text NOT NULL,
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0),
    framed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, run_id)
);

CREATE INDEX terminal_framed_age ON control.terminal_framed (instance_id, framed_at);
