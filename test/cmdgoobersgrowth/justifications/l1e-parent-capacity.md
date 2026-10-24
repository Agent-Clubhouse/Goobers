# L1e follow-up: process-wide holder for the per-epoch parent capacity (#8085 d)

`controlparentcapacity.go` adds one small type, `parentCapacity`: the engine
child protocol is built once, but the control role's scheduler (and the
`SchedulerCapacity` over it, which caches each parked parent's suspension bound
to that scheduler) is rebuilt every lease epoch. The holder is the protocol's
`ParentCapacity`; each epoch publishes its own capacity in Prepare and retires
it (only if still current) in Stop, so a successor never inherits the deposed
epoch's suspensions and Park/Complete refuse as not composed while none is
published. No new command or flag; the logic is the three delegating methods and
an atomic pointer, in cmd/ only because it binds the process-wide protocol to the
per-epoch admission.
