-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- The v0.6.0 stable release uses claim_run_history exclusively. All alpha
-- pods must be stopped before migrating; alpha.6 only knows claim_history.
-- Migration 0022 seeds its released incarnations and live leases into the
-- per-run store before this contract runs in the same upgrade.
DROP TABLE control.claim_history;
