# Selected PR repair host custody

The command package composes its existing claims lock, live runner registry,
per-gaggle worktree managers and archived run inventory for one bounded session
repair. Those instance-owned dependencies belong at the daemon boundary. Provider
CAS, typed intent, durable receipts, source authorization and tools remain in
their reusable packages. New files keep host custody and inventory checks separate
from service semantics; no second Git engine or credential resolver is introduced.
