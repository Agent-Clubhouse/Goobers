# Fix pod-stage and missing-config handling in PR branch occupancy discovery

`prSelectBranchOccupancies` guarded pod execution on GOOBERS_POD_TOKEN, which
dispatch-exec strips from every stage environment, so pr-select on a pod read a
nonexistent instance.yaml and failed every merge-review run. The growth is a
small pod-detection helper keyed on the dispatcher-owned GOOBERS_POD_ATTEMPT
identity that CLI stages keep, plus an ErrNotExist fallback, and regression
tests using the real stage environment. It stays in `cmd/goobers` beside the
existing occupancy wiring.
