-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- The pins of each started engine run, written by the start fence in the same
-- fenced transaction that confirms or adopts the Temporal start. The values
-- are the RunInput the Temporal workflow was started with (input_digest is the
-- start-owner memo digest), so later control-side checks need not trust the
-- run identity the worker writes into its journal. Rows are immutable and are
-- removed with their acceptance. A pin the started input left empty is
-- recorded empty; consumers that need it refuse their own operation.
CREATE TABLE control.run_pin (
    instance_id text NOT NULL,
    run_id text NOT NULL,
    acceptance_id text NOT NULL,
    gaggle text NOT NULL,
    workflow text NOT NULL,
    workflow_version integer NOT NULL,
    workflow_digest text NOT NULL,
    goober_digest text NOT NULL,
    config_generation text NOT NULL,
    repo_ref jsonb NOT NULL,
    branch_namespace text NOT NULL,
    input_digest text NOT NULL,
    owner_epoch bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (instance_id, run_id),
    UNIQUE (instance_id, acceptance_id),
    FOREIGN KEY (instance_id, acceptance_id)
        REFERENCES control.acceptance (instance_id, id) ON DELETE CASCADE
);
