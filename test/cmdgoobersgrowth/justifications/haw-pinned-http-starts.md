# LAND-E01: capture ordinary HTTP starts

The daemon owns the applied configuration catalog, generation archives, reload
publication lock, scheduler lifetime, provider authority and existing trigger
receipts. Thin ordinary-start adapters bind these existing owners together:
capture the applied target under an archive lease, rebuild that target through
the current generation compiler, preserve HTTP identity and audit semantics,
and protect accepted generations during startup before queue wiring exists.

Reusable envelope validation, idempotent acceptance, dispatch custody, journal
identity verification and generation inventory live in `internal/startintent`.
SQL remains in `internal/triggerqueue`; current admission remains in
`internal/localscheduler`. No duplicate scheduler, new credential owner, public
endpoint or alternative queue is introduced.

This slice connects live-daemon HTTP manual and priority starts only. Tests use
the actual handler, archive compiler, reloader, scheduler and journal. Separate
recovery tests cover typed/legacy receipts and replay only after startup-proven
absence. Standalone, targeted-PR, scheduled, signal and direct-engine adapters
remain separate landing work. The informational baseline is unchanged.
