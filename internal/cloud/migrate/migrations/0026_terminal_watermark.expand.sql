-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- The terminal awaiter's closed-run watermark: every run that closed at or
-- before closed_through has had its terminal frame fired by some leader. A
-- successor lists closed runs after the watermark (less a skew margin), so a run
-- that closed while no leader was awaiting still gets its frame. Only the live
-- controller lease may move it, and only forward.
CREATE TABLE control.terminal_watermark (
    instance_id text NOT NULL PRIMARY KEY,
    closed_through timestamptz NOT NULL,
    owner_epoch bigint NOT NULL CHECK (owner_epoch > 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE FUNCTION control.terminal_watermark_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.closed_through := GREATEST(OLD.closed_through, NEW.closed_through);
    NEW.owner_epoch := GREATEST(OLD.owner_epoch, NEW.owner_epoch);
    RETURN NEW;
END;
$$;

CREATE TRIGGER terminal_watermark_guard BEFORE UPDATE ON control.terminal_watermark
    FOR EACH ROW EXECUTE FUNCTION control.terminal_watermark_guard();
