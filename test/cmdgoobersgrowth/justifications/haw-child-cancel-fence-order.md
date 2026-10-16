# Child cancellation delivery ordering (LAND-C06)

The command layer composes the durable family fence, physical child executor and
queue-owned recovery. Recheck the exact retained child after its worker returns
so a fence observed before CancelRun delivery cannot become a failed outcome.
The existing queue owner verifies stopped writers and finalizes cancellation.
A parked cancellation remains pending delivery until independent result recovery
succeeds. No new state store, execution backend or terminal-history rewrite is
introduced; these lines connect the existing daemon-owned services.
