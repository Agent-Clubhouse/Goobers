# Cloud scheduler entries composition (#7818, L1b)

Growth: +14 non-test lines and +0 files in `cmd/goobers-cloud`, in
`controlassembly_config.go`.

## Why the growth belongs in the command package

The lines compose the control role's apply-time assessor: the existing
readiness assessor plus `schedentries.Assess`, so a generation whose triggers
the cloud scheduler could not tick is refused when it is applied rather than at
the first tick. Choosing which assessors the control role applies, and in what
order, is composition-root policy. The rule itself (trigger parsing, the
`TRIGGER001` finding, the workflow-keyed refusal) lives in
`internal/cloud/schedentries` and is tested there; this file only calls it and
adapts the finding shape to the `config` assessor interface.

## Could any of it live elsewhere?

The adaptation could move into `internal/cloud/schedentries` if a second
composition root ever needed it. There is one today, so a shared helper would
have a single caller.
