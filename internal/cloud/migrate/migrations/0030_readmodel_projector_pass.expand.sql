-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- The projector records its last completed catch-up pass in the model so the
-- API role can tell a live projector from a dead one. New read-model versions
-- get the same columns from the rebuild DDL.
ALTER TABLE readmodel_v1.meta ADD COLUMN IF NOT EXISTS last_pass_at timestamptz;
ALTER TABLE readmodel_v1.meta ADD COLUMN IF NOT EXISTS open_failures integer NOT NULL DEFAULT 0 CHECK (open_failures >= 0);
