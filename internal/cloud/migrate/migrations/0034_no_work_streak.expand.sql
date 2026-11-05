-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- The repeated-no-work counter (#5379), the cloud counterpart of the daemon's
-- per-item no-work-streak records. It is kept apart from the failure streak:
-- a no-work run completes, and completion resets the failure streak, so the two
-- counters have opposing reset triggers. A productive completion deletes the row.
CREATE TABLE control.no_work_streak (
    instance_id text NOT NULL,
    repo_key text NOT NULL CHECK (repo_key <> ''),
    item_id text NOT NULL CHECK (item_id <> ''),
    record jsonb NOT NULL,
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0),
    -- Runs that already advanced this record; a re-fired frame never counts twice.
    recent_runs text[] NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, repo_key, item_id)
);
