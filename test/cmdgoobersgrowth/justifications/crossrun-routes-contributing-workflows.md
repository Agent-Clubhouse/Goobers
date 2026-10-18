# Conflict-touches and unpushed-work consider only workflows that can contribute

Growth: +31 non-test lines and +0 files in `cmd/goobers`
(`journalreadplane.go`, plus one wiring line in `up.go`).

## Why the growth belongs in the command package

- **`contributingWorkflows`.** The daemon's current workflow definitions live in
  the command package (`interventionDefinitionRegistry`), and the handlers need
  the gaggle's workflow names that satisfy a predicate. It is one loop over the
  registry snapshot. It refuses when no definitions are attached, so the scan can
  never silently widen back to every workflow.
- **Two call sites and one wiring line.** Each handler derives its workflow set
  and hands it to `journalclient`; `goobers up` attaches the registry.

## Could any of it live elsewhere?

No more of it than already does. The predicates that decide which workflows can
produce what each route reads (`WorkflowCanRecordBaseSyncConflict`,
`WorkflowCanStrandUnpushedWork`), the per-workflow read-model listing and both
scan bodies live in `internal/journalclient`, next to the artifact-name
constants they mirror.
