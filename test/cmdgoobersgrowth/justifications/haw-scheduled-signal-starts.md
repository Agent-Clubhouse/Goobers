# HAW-EVT-002: durable scheduled and signal source adapters

The command package wires the existing generation archive into the reusable
startintent source adapter, enables durable webhook acknowledgement in the
existing listener, and transfers the existing standalone signal command into
the same trigger ledger. The new signalsource.go is limited to assembling
private scheduler/daemon services and preserving the CLI's wait/output behavior.
No additional queue, listener, or background loop is introduced.

Cursor/source transactions and quotas belong to internal/triggerqueue; pinned
source construction belongs to internal/startintent; scheduler occurrence,
backoff, and prepared admission belong to internal/localscheduler. Actual
handler, archive, Runner, and CLI replay tests cover the production wiring.
