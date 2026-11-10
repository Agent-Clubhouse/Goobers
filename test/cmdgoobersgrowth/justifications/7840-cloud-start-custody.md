# HA-P3-07 cloud start custody composition

The cloud command supplies a Temporal starter factory backed by the concrete PostgreSQL acceptance store, a lease-binding hook, and recovery/status hooks. Policy remains in internal/cloud/acceptance and internal/cloud/startfence. The local goobers command gains no code and no PostgreSQL or Azure Blob dependencies.

HA-P3-18 #7818 connects the controller loop, lease binding, pinned-input resolver and status endpoint. Until then the cloud role remains unavailable and the starter refuses starts without a bound live lease. No continue/child admission endpoints are fabricated in this change.
