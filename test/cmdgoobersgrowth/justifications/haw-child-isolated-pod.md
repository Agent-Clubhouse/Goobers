# HAW-CHD contained worker transport

This slice adds a dedicated branch to the existing dispatch entrypoint for a
child contract, private environment setup, isolated workspace materialization,
bounded result publication and surrender. `cmd/goobers` owns these existing
process and HTTP-client composition seams. Tree carriers and pod containment
remain in `internal/childpod`, `internal/recovery` and `internal/dispatcher`.

The host factory, authenticated child blob plane, result application and recovery
are separate follow-ups. No public child execution path is enabled by this slice.
The dispatcher requires a dedicated child token minter; the ordinary minter is
insufficient. Linux worker entry checks require the private PID namespace; other
substrates and synthetic-history PR publication refuse. No baseline is raised.
