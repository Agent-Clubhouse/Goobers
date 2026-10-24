# #7818: terminal-hook composition into the epoch

Growth: about +260 non-test lines and +1 production file in `cmd/goobers-cloud`
(`controlterminal.go`), plus small edits to `controlepoch.go` and `control.go`
(the shared scheduler-log opener's own +89 is justified in `7818-scheduler-log.md`).

The hook policies, state, awaiter and delivery loop all live in `internal/`
(`internal/cloud/terminalcompose` and the packages it assembles). What stays in
the command package is the part that cannot: reading the control role's own
seams (the claim plane, configuration owner, journal backend, acceptance store,
retained generations, the dialed Temporal client and the process reporter) and
handing them to `terminalcompose.New` once per lease epoch, plus the process-wide
publication of the outbox status for the readiness probe and the status page.
Those seams are command-package types (`controlPlanes`, `databaseServices`), so
the adapter cannot move into `internal/` without importing `package main`.
