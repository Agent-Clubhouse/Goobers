# #8375: Labs fleet workflow-feature snapshots

Growth: a small `cmd/goobers` helper, `labsWorkflowFeaturesOptions`
(`cmd/goobers/fleet.go`), and one call in each of `goobers up`
(`configureAPI`) and the standalone dashboard (`standaloneDashboardAPI`).

## Why the growth belongs in the command package

- The helper binds the command layer's existing fleet-storage seam
  (`newFleetStorage`, which the `goobers fleet` subcommands already own) to
  the instance API. Both the daemon and the standalone dashboard wire the same
  optional route through it.
- It also owns the startup policy: if fleet storage cannot be initialized, it
  logs a warning and leaves the route unconfigured. The API then answers 503
  `labs_unavailable` and startup continues. Startup policy is decided at this
  composition boundary.

## What lives elsewhere

The association gating, snapshot construction and privacy-safe hashing live in
`internal/workflowcluster` (`FleetSnapshotSource`). The route, query
validation, audit logging and capability availability live in
`internal/httpapi`. No CLI command or flag is added.
