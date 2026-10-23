-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

DO $$ BEGIN CREATE ROLE goobers_api NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $$;
DO $$ BEGIN CREATE ROLE goobers_controller NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $$;

GRANT USAGE ON SCHEMA control TO goobers_api, goobers_controller;

REVOKE INSERT, UPDATE, DELETE ON control.lease FROM goobers_api, goobers_controller;
GRANT INSERT (instance_id, name, holder, epoch) ON control.lease TO goobers_controller;
GRANT UPDATE (holder, epoch) ON control.lease TO goobers_controller;

REVOKE INSERT, UPDATE, DELETE ON control.lease_heartbeat FROM goobers_api, goobers_controller;
GRANT INSERT (instance_id, name, holder, epoch, expires_at) ON control.lease_heartbeat TO goobers_controller;
GRANT UPDATE (holder, epoch, expires_at) ON control.lease_heartbeat TO goobers_controller;

REVOKE INSERT, UPDATE, DELETE ON control.lease_history FROM goobers_api, goobers_controller;
GRANT INSERT (instance_id, name, epoch, holder, acquired_at) ON control.lease_history TO goobers_controller;
GRANT UPDATE (released_at, release_kind) ON control.lease_history TO goobers_controller;

REVOKE INSERT, UPDATE, DELETE ON control.readmodel_version FROM goobers_api, goobers_controller;
GRANT INSERT (name, active_version, updated_at) ON control.readmodel_version TO goobers_controller;
GRANT UPDATE (active_version, updated_at) ON control.readmodel_version TO goobers_controller;

REVOKE INSERT, UPDATE, DELETE ON control.schema_migration FROM goobers_api, goobers_controller;

