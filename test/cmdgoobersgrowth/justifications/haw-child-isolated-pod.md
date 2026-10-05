# HAW-CHD isolated pod composition

This slice adds a dedicated branch to the existing dispatch entrypoint for an
authenticated child contract, clean environment setup, isolated workspace
materialization, bounded result publication and surrender. `cmd/goobers` owns
these existing process and HTTP-client composition seams. Tree transport,
application planning, pod containment and bounded blob custody remain in
`internal/childpod`, `internal/recovery`, `internal/dispatcher` and
`internal/triggerqueue`.

The child daemon never receives Kubernetes permissions. Dispatch is supplied by
the worker transport. Only a verified Linux private PID namespace may execute
this entrypoint; other substrates and synthetic-history PR publication refuse.
