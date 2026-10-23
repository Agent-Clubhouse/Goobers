-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- The pins of each started engine run, written by the start fence in the same
-- fenced transaction that confirms or adopts the Temporal start. The values
-- are the RunInput the Temporal workflow was started with (input_digest is the
-- start-owner memo digest), so later control-side checks need not trust the
-- run identity the worker writes into its journal. Rows are immutable except
-- terminal_at, and outlive their acceptance row (see terminal_at). A pin the started input left empty is
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
    -- terminal_at is set once the run's engine execution is known closed; a
    -- row is removed only ReplayRetention after that. There is deliberately
    -- no foreign key to control.acceptance: the acceptance row is pruned
    -- ReplayRetention after the START finished, while the run (and its child
    -- families' bindings) can still be live.
    terminal_at timestamptz,
    PRIMARY KEY (instance_id, run_id),
    UNIQUE (instance_id, acceptance_id)
);

CREATE INDEX run_pin_retention_idx
    ON control.run_pin (instance_id, terminal_at, created_at);
