-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

ALTER TABLE control.acceptance
    ADD COLUMN dispatch_started_at timestamptz,
    ADD COLUMN start_retry_at timestamptz,
    ADD COLUMN start_retry_count integer NOT NULL DEFAULT 0 CHECK (start_retry_count BETWEEN 0 AND 6);

-- Existing uncertain rows enter recovery without asserting ownership.
UPDATE control.acceptance SET dispatch_started_at=accepted_at WHERE state='dispatching';
CREATE INDEX acceptance_start_recovery_idx
    ON control.acceptance (instance_id, start_retry_at, dispatch_started_at)
    WHERE state='dispatching';
