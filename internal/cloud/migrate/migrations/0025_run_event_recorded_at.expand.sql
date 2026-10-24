-- goobers:binary-version 0.7.0
-- goobers:min-compatible-binary 0.6.0

-- The database-clock instant each run event was committed, for stall
-- accounting that must not trust a run writer's clock (event bodies carry the
-- writer's Time). Added nullable without a default, which needs no table
-- rewrite, then defaulted to clock_timestamp() so every later insert is
-- stamped, including a previous binary's, which never names the column. Rows
-- written before this migration keep NULL: readers fall back to the writer
-- clock for them. clock_timestamp() is the serving primary's clock, so across
-- a failover (CNPG promotes a replica) the stamps of one run can step slightly
-- backwards; they are not a strict order.
ALTER TABLE journal.run_event ADD COLUMN recorded_at timestamptz;
ALTER TABLE journal.run_event ALTER COLUMN recorded_at SET DEFAULT clock_timestamp();
