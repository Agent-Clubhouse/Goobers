# Labs: \goobers workflow clusters --features behavior\ (#8303)

Growth: four completion-model entries in \cmd/goobers/completionmodel.go\ for
the new \--features\, \--since\, \--max-runs\ and \--min-samples\ flags of
the existing \workflow clusters\ subcommand, plus an updated synopsis, help
summary and example in \cmd/goobers/workflow.go\.

All behavior (bounded read-only journal scanning, behavior descriptor
extraction, EffectiveVersion cohort aggregation, clustering and report
rendering) lives in \internal/workflowcluster\. The command layer only
declares the flags for shell completion and forwards arguments and streams.
There is nothing left to extract.
