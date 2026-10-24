# #8192: durable cycle deferral driver and instance-level escalator

Growth: about +12 non-test lines in `cmd/goobers-cloud` (`controlterminal.go`).

The deferral store, the sweep that drives it, the escalation policy and the
locked-down instance-level writer all live in `internal/` (`blockedrecordspg`,
`terminalcompose`, `terminalwriter.InstanceWriter`, `parking.EscalateCycle`). The
command package only builds the writer from the control role's own seams (the
applied configuration, the credential backends and the epoch's fenced log) and
passes it into the composition, which is the same adapter role the rest of
`controlterminal.go` already has; those seams are command-package types, so the
construction cannot move into `internal/`.
