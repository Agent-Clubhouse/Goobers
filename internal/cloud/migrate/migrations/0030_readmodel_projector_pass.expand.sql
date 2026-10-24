-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- The projector records its last completed catch-up pass in the model so the
-- API role can tell a live projector from a dead one. Every existing read-model
-- version schema gets the columns (a version built by an older binary lacks
-- them); new versions get them from the rebuild DDL.
DO $$
DECLARE
    model text;
BEGIN
    FOR model IN
        SELECT n.nspname FROM pg_namespace n
        WHERE n.nspname ~ '^readmodel_v[1-9][0-9]*$'
          AND to_regclass(format('%I.meta', n.nspname)) IS NOT NULL
    LOOP
        EXECUTE format('ALTER TABLE %I.meta ADD COLUMN IF NOT EXISTS last_pass_at timestamptz', model);
        EXECUTE format('ALTER TABLE %I.meta ADD COLUMN IF NOT EXISTS open_failures integer NOT NULL DEFAULT 0 CHECK (open_failures >= 0)', model);
    END LOOP;
END
$$;
