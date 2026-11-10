-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- A dependency cycle whose members could not be verified with the provider is
-- not escalated on a guess and not parked forever: the seed record (the item whose
-- block closed the cycle) carries a deferred marker, and the controller's sweep
-- re-checks it when verify_after (database clock) falls due, so it survives a
-- failover. The cycle's members are recomputed from the records at sweep time, so
-- no member list is stored. deferred_since is when it was first deferred; it
-- bounds how long a cycle may wait before it is escalated as unverifiable.
ALTER TABLE control.blocked_record
    ADD COLUMN verify_after timestamptz,
    ADD COLUMN deferred_since timestamptz,
    ADD COLUMN defer_attempts integer NOT NULL DEFAULT 0 CHECK (defer_attempts >= 0),
    ADD COLUMN defer_reason text NOT NULL DEFAULT '',
    ADD CONSTRAINT blocked_record_deferral_pair CHECK ((verify_after IS NULL) = (deferred_since IS NULL));

CREATE INDEX blocked_record_due ON control.blocked_record (instance_id, verify_after) WHERE verify_after IS NOT NULL;
