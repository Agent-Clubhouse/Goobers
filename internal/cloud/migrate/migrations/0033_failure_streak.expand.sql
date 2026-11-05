-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- The circuit breaker's consecutive-failure counters, the cloud counterpart of
-- the daemon's failure-streak records: one row per item and streak kind (the
-- genuine/work streak, and the separately thresholded infrastructure streak).
-- A reset deletes the row, so only items with a running streak are stored.
CREATE TABLE control.failure_streak (
    instance_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('work', 'infra')),
    repo_key text NOT NULL CHECK (repo_key <> ''),
    item_id text NOT NULL CHECK (item_id <> ''),
    count integer NOT NULL CHECK (count > 0),
    run_id text NOT NULL,
    stage text NOT NULL DEFAULT '',
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0),
    -- The most recent runs that advanced this streak. A terminal frame can be
    -- re-fired by a successor leader; a run already listed here never charges the
    -- item a second time.
    recent_runs text[] NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, kind, repo_key, item_id)
);
