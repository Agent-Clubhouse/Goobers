# Workflow Gym offline historical replay (#8352)

`goobers workflow-gym replay <task-manifest> <provider-fixture> <journal-dir>`
adds one subcommand to the existing `workflow-gym` group. The command layer is
a thin wrapper in `cmd/goobers/workflowgym.go`: it checks the argument count,
loads the single task manifest with `workflowgym.Load`, calls
`workflowgym.Replay`, and encodes the resulting replay manifest as JSON. The
replay itself (fresh workspaces, the recorded-session act driven through the
benchmark sandbox, the sandbox-observed effect ledger, and the nondeterminism
comparison) lives in `internal/workflowgym` on top of `internal/harness`.
