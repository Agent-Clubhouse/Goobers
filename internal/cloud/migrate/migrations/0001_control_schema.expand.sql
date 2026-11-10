-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

CREATE SCHEMA IF NOT EXISTS control;

CREATE TABLE IF NOT EXISTS control.schema_migration (
    version integer PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('expand', 'contract')),
    binary_version text NOT NULL,
    min_compatible_binary text NOT NULL,
    backup_ref text NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS control.readmodel_version (
    name text PRIMARY KEY,
    active_version integer NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
