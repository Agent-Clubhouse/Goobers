# HA-P3 migration lease identity

Issue #7803 adds the instance and process identity needed for the migration
role to acquire the shared PostgreSQL lease. The lease, heartbeat, takeover,
self-fence, and transaction-fence behavior lives under `internal/cloud/lease`;
`cmd/goobers-cloud` grows only to source and pass the two identifiers.
