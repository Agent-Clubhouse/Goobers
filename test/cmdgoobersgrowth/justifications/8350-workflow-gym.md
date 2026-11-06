# Workflow Gym benchmark task registry (#8350)

`goobers workflow-gym validate|list <path>` adds a new command group. The
command layer is a thin wrapper: `cmd/goobers/workflowgym.go` only parses the
single path argument, calls `workflowgym.Load`, and prints the dataset version,
digest, task count, and task list. The command registration is built by
`workflowGymCLICommand()` so `init` grows by a single line. Manifest decoding,
schema validation, split and provenance checks, duplicate detection, stable
task ordering, and the deterministic dataset digest all live in
`internal/workflowgym`.