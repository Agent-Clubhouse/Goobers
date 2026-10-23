# HA-P1-13 cloud worker composition

Issue #7723 moves the worker composition root, including the remote config
plane, generation leasing, and live updater, from `cmd/goobers` into
`cmd/goobers-cloud`. The cloud command grows because it now owns that role;
the corresponding `goobers worker` command and generated surfaces are removed.
Worker behavior tests move with their subjects, while executor, credential,
preflight, identity, and recovery wiring is shared in `internal/runtimewiring`.
