# Control-role read-model projector (#7818)

`goobers-cloud control` starts the read-model projector as a third lifetime
service next to the controller host and key reload. The new command-layer code
only binds existing owners: the lease manager (a second, "projector"-named lease
so projection can never fence admission), the journal plane's committed-history
reader, and `internal/cloud/projectorpg`, which holds the catch-up and commit
logic. The command layer adds the service constructor and the journal-observer
nudge (a one-slot channel); `runControlLifetime` gains a variadic background
service list instead of a second bespoke goroutine group.
