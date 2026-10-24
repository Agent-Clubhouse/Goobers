-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- Shared custody survives an uncertain remote acknowledgment and controller
-- takeover. The deadline is the remote CAS deadline, never a local extension.
-- Defaults preserve local claims. Admission remains disabled until composition.
ALTER TABLE control.claim
    ADD COLUMN shared_owner jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN shared_deadline timestamptz,
    ADD COLUMN shared_admission_pending boolean NOT NULL DEFAULT false,
    ADD COLUMN shared_revoked boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT claim_shared_deadline CHECK (
        (shared_deadline IS NULL AND shared_owner = '{}'::jsonb
            AND NOT shared_admission_pending AND NOT shared_revoked)
        OR (shared_deadline IS NOT NULL AND shared_deadline = expires_at
            AND shared_owner <> '{}'::jsonb)
    );

ALTER TABLE control.claim_run_history
    ADD COLUMN shared_owner jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN shared_deadline timestamptz,
    ADD COLUMN shared_admission_pending boolean NOT NULL DEFAULT false,
    ADD COLUMN shared_revoked boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT claim_run_history_shared_deadline CHECK (
        (shared_deadline IS NULL AND shared_owner = '{}'::jsonb
            AND NOT shared_admission_pending AND NOT shared_revoked)
        OR (shared_deadline IS NOT NULL AND shared_deadline = expires_at
            AND shared_owner <> '{}'::jsonb)
    );

ALTER TABLE control.claim_history
    ADD COLUMN shared_owner jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN shared_deadline timestamptz,
    ADD COLUMN shared_admission_pending boolean NOT NULL DEFAULT false,
    ADD COLUMN shared_revoked boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT claim_history_shared_deadline CHECK (
        (shared_deadline IS NULL AND shared_owner = '{}'::jsonb
            AND NOT shared_admission_pending AND NOT shared_revoked)
        OR (shared_deadline IS NOT NULL AND shared_deadline = expires_at
            AND shared_owner <> '{}'::jsonb)
    );

