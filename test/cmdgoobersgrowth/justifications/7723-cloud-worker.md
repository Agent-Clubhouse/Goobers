# HA-P1-13 cloud worker composition

Issue #7723 moves the complete worker composition root, including the remote
config plane, generation leasing, and live updater, from `cmd/goobers` into
`cmd/goobers-cloud`. The cloud command grows because it now owns that role;
the corresponding `goobers worker` command, wiring, tests, and generated
surfaces are deleted.
