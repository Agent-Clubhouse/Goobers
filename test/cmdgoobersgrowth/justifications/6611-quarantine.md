# #6611: Quarantine bad goober instruction sources

The command package gains daemon scheduler quarantine logic because the failure
mode is specific to command-owned startup wiring: a goober instruction file can
become unreadable after config validation but before runtime construction. The
daemon must convert that late read failure into per-workflow startup refusals,
instance-log diagnostics, and scoped runner construction so healthy gaggles
keep serving.

This cannot live in lower-level validation or workflow packages without
changing their fail-closed contract. CLI validation, status, preflight, worker
digest checks, and non-daemon callers still use the existing strict instruction
loader; only scheduler construction gets the partial-load quarantine behavior.
