# HAW-HITL-004: explicit restart retry and source-owned replay retention

This slice adds daemon composition for bounded restart replay compaction and
qualified source retirement. The host already owns the runner registry,
instance run-root inventory, and telemetry prune guard needed to prove that a
source journal has been irreversibly retired. Those host dependencies stay in
`cmd/goobers`; the transactional occurrence guard, compact replay storage,
principal/command matching, and maintenance coordination live in
`internal/triggerqueue`, `internal/restartintent`, and `internal/startcontrol`.
No new command or independent persistence store is introduced.
