# HAW-CHD-003 production child launcher composition

This slice installs the durable child launcher, exact-source generation recovery,
terminal custody observer and credential-policy verifier in daemon startup. The
new command code composes the daemon's existing scheduler, queue, retained-config
builder, credential service and run registry. Those objects are owned by the
command package; moving this wiring below it would invert dependencies.

Reusable isolation of the generated run's driver and factories lives in
`internal/runner`. Pod execution, transport and container termination remain in
the isolated adapter. The command factory seam admits its exact retained inputs;
absence defers durable intent instead of falling back to local execution.
