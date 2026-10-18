# backprop/fix-6571: reap terminal held stage pods

Growth: +6 non-test lines and +0 files in `cmd/goobers`, in `workersweep.go`.

## Why the growth belongs in the command package

The reaping decision itself is in `internal/dispatcher` (`orphan.go`). The
worker's sweep loop now calls `SweepOrphansWithReport` and prints one line per
pod it disposes of, with the reason. That gives an operator the reason a pod in
Error state disappeared. Logging the sweep is the worker command's job, so the
loop is the right place.

## Could any of it live elsewhere?

No. The six lines are the log loop over the dispatcher's report.