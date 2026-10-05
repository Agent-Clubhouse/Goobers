# HAW-CHD-003 daemon composition

This slice connects the existing daemon endpoint, signing key, trigger queue,
applied configuration and local runner factory to child-workflow authority.
These host-owned objects are assembled in `cmd/goobers`; the runtime, admission,
grant lifecycle, policy comparison and durable authority logic remain in
`internal/childworkflow` and `internal/triggerqueue`.

The new command file contains composition callbacks and session cleanup only.
Existing startup/reload and runner construction sites select those callbacks.
Moving this wiring below the command package would invert dependencies on its
configuration-generation builder and daemon credential registry.
