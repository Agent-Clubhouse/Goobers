# Child workflow proposal pattern map flags (#8309)

`goobers workflow clusters` gains `--proposals` and `--gaggle` completion
entries (two lines in `completionmodel.go`). The command layer only exposes the
flags; proposal scanning, per-gaggle fitting, and JSON shaping live in
`internal/workflowcluster`, shared with the Portal Labs read contract.
