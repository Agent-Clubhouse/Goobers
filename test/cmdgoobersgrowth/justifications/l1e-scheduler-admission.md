# L1e: control-role scheduler admission composition (#7818)

`goobers-cloud control` gains the composition of the shared scheduler's admission
for accepted cloud starts. The new command-layer files only bind existing
owners; the logic lives in `internal/cloud/schedstart` (admission path),
`internal/cloud/schedentries` (entries from the applied generation),
`internal/cloud/journalpg` (the scheduler's fenced instance log),
`internal/cloud/acceptance` (open-run pins) and `internal/localscheduler`.

- `controlscheduler.go`: the per-epoch `control.Admission` (Prepare, Tick, Stop)
  and the readiness record of scheduler gates that are not enforced.
- `controlscheduler_openruns.go`: merges the start fence's run pins (confirmed
  against Temporal) with unresolved dispatching starts into the slot holders a
  successor epoch reconciles against.
- `controlscheduler_wire.go`: the seam constructor that binds the process-wide
  parts (applied configuration, journal backend, claims services, Temporal
  client, one shared provider quota state).
- `controlobservations.go`: the observations ledger over that same quota state.
