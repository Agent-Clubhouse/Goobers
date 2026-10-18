# #6649: worker blob-probe waits for daemon readiness

Growth: about +11 non-test lines in `cmd/goobers` (`worker.go`, `workerblob.go`,
`completionmodel.go`).

## Why the growth belongs in the command package

- `worker.go` gains the `--blob-probe-wait` flag (env
  `GOOBERS_WORKER_BLOB_PROBE_WAIT`, default 20m) and a six-line
  `workerEnvDuration` helper beside `workerEnvOr`. Flag parsing and env
  defaults for `goobers worker` live only in this command.
- `workerblob.go` threads the resulting `workerblob.ProbeOptions` (wait bound
  and stderr progress log) into `workerblob.Open`.

- `completionmodel.go` and `clisynopsis.go` register the new flag; a test
  requires handler flags, synopsis and completions to agree.

## Could any of it live elsewhere?

All classification, backoff and logging logic is in `internal/workerblob`
(and the typed errors in `internal/dispatcher`); only flag/env wiring remains
in `cmd/goobers`.
