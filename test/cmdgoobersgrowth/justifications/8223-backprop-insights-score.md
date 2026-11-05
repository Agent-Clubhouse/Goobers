# Native Backprop Insights: preserve signed scores across telemetry adapters

Growth: +2 non-test lines and +0 files in `cmd/goobers`, in
`telemetrydefectplane.go` and `telemetryquery.go`.

## Why the growth belongs in the command package

The existing wire and plane conversion functions each need one `Score` field
assignment. Without either assignment, remote attribution cohorts silently lose
signed scores, including genuine zero, even though local cohorts retain them.
These are adapter wiring changes, not new attribution logic.

## Could any of it live elsewhere?

Aggregation and ground-truth weighting remain in `internal/creditgraph`; the
remote DTO remains in `internal/telemetryclient`. The two existing command-owned
conversion literals must copy the optional field in both directions. Moving
those adapters would be unrelated refactoring rather than reducing this change.
