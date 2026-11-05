# Standalone dashboard serves Labs workflow clusters (#8309)

Growth: one import and one handler option in `cmd/goobers/dashboard.go` that
wire the existing read-only `workflowcluster.ConfiguredSource` into the
standalone dashboard API, matching the identical option `goobers up` already
passes. The source and route live in `internal/workflowcluster` and
`internal/httpapi`; nothing further can move out of `cmd/goobers`.