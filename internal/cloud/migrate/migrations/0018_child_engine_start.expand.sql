-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- One engine-owned start intent per accepted child: the frozen manifest it
-- runs, the Temporal parent execution that owns its start, and its pinned
-- wait deadline. A row means a Temporal parent owns the start; runner
-- launchers never start such a child. Rows are immutable after insert except
-- for owner_epoch, which follows lease takeover.
CREATE TABLE control.child_engine_start (
    instance_id text NOT NULL,
    child_id text NOT NULL,
    manifest_digest text NOT NULL,
    parent_workflow text NOT NULL,
    parent_workflow_id text NOT NULL,
    parent_temporal_run_id text NOT NULL,
    deadline timestamptz NOT NULL,
    owner_epoch bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (instance_id, child_id),
    FOREIGN KEY (instance_id, child_id)
        REFERENCES control.child_lineage (instance_id, child_id) ON DELETE CASCADE
);
