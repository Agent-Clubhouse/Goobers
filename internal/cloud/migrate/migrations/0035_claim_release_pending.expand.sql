-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- Cleanup custody for provider-cleanup claim releases, the file ledger's
-- ReleasePending: set when the release reserves the exact incarnation, cleared
-- only by finishing that release after the provider marker is removed. While
-- it is set no run may take, renew or plainly release the claim, and expiry
-- recovery skips it. Previous binaries neither read nor write the column.
ALTER TABLE control.claim ADD COLUMN release_pending_at timestamptz;
ALTER TABLE control.claim_run_history ADD COLUMN release_pending_at timestamptz;
