# HAW-EVT-007: event dependencies at existing host prune boundaries

The command package adds two narrow adapters to its existing journal-prune guard
and configuration-generation inventory. These must run before daemon composition
and from standalone retention commands, so they open the instance-owned trigger
database at the existing host layout. The reusable, bounded dependency inventory
lives in `internal/eventexecution`; transaction ownership remains in
`internal/triggerqueue`. No additional command or retention store is introduced.
