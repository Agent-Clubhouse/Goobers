# HA-P3 cloud PostgreSQL composition

Issue #7801 adds the database flags and lifecycle glue required for the
`goobers-cloud` API and controller roles. The reusable pool and authentication
behavior lives under `internal/cloud`; `cmd/goobers-cloud` grows only for flag
parsing, dependency construction, error reporting, and role shutdown.
