-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- A predecessor start gets one immediate attempt after its owner loses authority;
-- unresolved starts retain retry backoff even across later lease epochs.
ALTER TABLE control.acceptance
    ADD COLUMN start_retry_epoch bigint NOT NULL DEFAULT 0 CHECK (start_retry_epoch >= 0);

