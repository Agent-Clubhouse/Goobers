# Labs: workflow cluster map API wiring (#8309)

Growth: one `internal/workflowcluster` import in `cmd/goobers/up.go` so the
daemon can pass `httpapi.WithLabsWorkflowClusters(workflowcluster.ConfiguredSource{...})`
alongside the existing gaggle health option.

The read model, route handler and portal view live in `internal/workflowcluster`,
`internal/httpapi` and `portal/`. The command layer only wires the configured
instance root into the daemon's handler options; there is nothing left to extract.
