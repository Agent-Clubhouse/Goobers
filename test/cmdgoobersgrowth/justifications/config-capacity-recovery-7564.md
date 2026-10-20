# Protected configuration capacity recovery (#7564)

The command package gains daemon startup recovery and its explicit operator
selection flag. It composes existing generation verification and leases with
CLI-owned configuration loading, scheduler setup, reload status, and the
instance journal. It also renders the legacy refusal diagnostic and retained
candidate list. These paths must share startup's definitions, diagnostic report,
and applied-catalog state rather than create a second daemon composition layer.

Storage capacity classification, retained-generation verification and process
leases remain in `internal/configgeneration`. The commit-safe reload event
remains in `internal/localscheduler`. The CLI does not implement a second store,
relax retention bounds, or introduce a generic archive restoration mechanism.
