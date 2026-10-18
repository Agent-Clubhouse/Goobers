# fix/pod-gocache-tmpfs

`cmd/goobers/workerdispatch.go` grows by five lines: the worker must read
`runner.podTmpfsSize` from the loaded instance config (which only this command
can read; a stage pod cannot) and hand it to `dispatcher.Config.TmpfsSizeLimit`,
the field that existed but had no caller. Parsing and validation live in
`internal/instance` (`ResolvePodTmpfsSize`); the pod-spec placement of `GOCACHE`
and the recovery-custody change live in `internal/dispatcher` and
`internal/recovery`. Only the config-to-dispatcher hand-off belongs in the
command package, beside the other `dispatcher.Config` fields.
