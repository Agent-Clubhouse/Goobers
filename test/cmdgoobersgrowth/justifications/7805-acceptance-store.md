# HA-P3 cloud acceptance composition

Issue #7805 wires the shared PostgreSQL acceptance store into the cloud
API/controller composition root. Acceptance, cursor, source receipt, demand,
retention, and uncertainty behavior remains under `internal/cloud/acceptance`;
command growth is limited to constructing that reusable store from the existing
database pool, instance identity, and current owner epoch.
