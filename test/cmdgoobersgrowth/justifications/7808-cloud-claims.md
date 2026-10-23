# HA-P3 cloud claims composition

Issue #7808 wires a live-lease-bound PostgreSQL claim and scheduler-state store
into the cloud API/controller composition root. Claim, merge-lock, history,
owner-epoch, state CAS, and PostgreSQL locking behavior remains under
`internal/cloud/claims`; command growth is limited to constructing that shared
store from the existing pool, instance identity, and acquired lease.
