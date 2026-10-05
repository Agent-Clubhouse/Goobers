# HAW-EVT-007: source-qualified queue control host adapters

This slice adds command composition for exact archived queue policy, installs the
shared queue inspection/cancellation service, and qualifies local cancellation
against existing daemon runner ownership, journal locks and contained workers.
Those dependencies belong to the daemon host. Reusable metadata, policy,
authority, receipt rendering and cancellation coordination live in
`internal/startcontrol`; transport lives in `internal/httpapi`. Existing source
dispatch calls only gain the explicit clock/control admission seam. The small
typed cancellation placeholder is replaced by the independently reviewed
child/session host adapters; it grants no effect or termination evidence.
