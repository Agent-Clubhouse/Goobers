# HAW-EVT-002: pinned ordinary start admission

The daemon now assembles the existing trigger ledger's typed ordinary start
service, captures targets from the applied catalog, opens exact retained
execution generations, and transfers same-root delegated files into that
service. The new command-package files are host adapters for existing private
scheduler/engine construction and the existing file protocol. No command,
server, independent store, or background loop is added.

Typed envelopes, duplicate handling, dispatch custody, journal reconciliation,
retained-generation inventory, and execution leases live in internal/startintent.
Current admission and targeted PR validation remain in internal/localscheduler.
The existing event runtime shares archive-definition loading and pin verification
with ordinary starts. Composed tests use actual HTTP handlers, file delegation,
archive compilation, scheduler admission, runner journals, and reopen recovery.
