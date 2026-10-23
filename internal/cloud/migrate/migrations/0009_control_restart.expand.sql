-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- A new lease gets one immediate reconciliation attempt for predecessor starts;
-- unresolved starts thereafter retain their durable retry backoff.
ALTER TABLE control.acceptance
    ADD COLUMN start_retry_epoch bigint NOT NULL DEFAULT 0 CHECK (start_retry_epoch >= 0);

