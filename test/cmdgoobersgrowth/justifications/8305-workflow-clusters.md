# Labs: `goobers workflow clusters` (#8305)

Growth: one read-only `workflow clusters` subcommand registration, its synopsis,
help text and a thin `runWorkflowClusters` wrapper in `cmd/goobers/workflow.go`,
plus one completion-model entry for its three flags.

All behavior (loading configured workflows, structural feature extraction,
HDBSCAN clustering, stability, and report rendering) lives in
`internal/workflowcluster`. The command layer only registers the verb and
forwards arguments and streams, matching the existing `workflow show` and
`workflow validate-child` wiring. There is nothing left to extract.
