# L1e: provider-backed counters in the control composition (#7818)

`controlscheduler_counters.go` composes the existing `internal/cloud/schedcounters`
(MaxOpenPRs, backlog, refill and schedule-demand) into the control role. The
command layer only binds existing owners: the applied configuration's repos and
secret stores through the same instance-level credential resolver the local
daemon uses (`runtimewiring.BuildCredentials`), the shared provider quota state,
the claims plane (`ListNamespace`) and the run-journal read plane. The one piece of
local logic is the `gateReporter`: counters that are composed but leave a gate
unenforced (schedule-demand without its inputs) keep exactly that gate on the
readiness checks and journal one warning per epoch. No new command or flag.
