-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- Worker-authenticated stop proof is sealed with authority revocation. Lost
-- activity completions can recover the same handoff without a fresh dispatch.
ALTER TABLE control.child_authority
    ADD COLUMN stopped_handoff jsonb,
    ADD CONSTRAINT child_stopped_handoff_size
        CHECK (stopped_handoff IS NULL OR octet_length(stopped_handoff::text) <= 16384);
