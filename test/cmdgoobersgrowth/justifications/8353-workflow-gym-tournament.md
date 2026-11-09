# Workflow Gym paired tournament runner (#8353)

`goobers workflow-gym tournament <dataset> <authoritative> <candidate> [candidate...]`
adds one subcommand to the existing `workflow-gym` group. The command layer
loads the dataset and candidate result manifests, then delegates canonical
task pairing, fail-closed missing-result handling, and per-slice aggregation to
the reusable `internal/workflowgym` tournament runner.
