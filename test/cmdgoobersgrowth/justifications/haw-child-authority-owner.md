# HAW-CHD contained child authority and recovery

This slice connects the daemon's retained child admission to its existing worker
transport. The command package owns the applied configuration, runner registry,
credential source, live journal writer, parent claim service and Temporal client;
the new adapters bind those existing owners without adding another scheduler or
credential store. Startup installs all child HTTP owners with the same backend.

Reusable contract validation, bounded artifact storage, transport reconciliation,
signed identity and runner custody handling remain in `internal/childpod`,
`internal/triggerqueue`, `internal/podauth` and `internal/runner`. Command adapters
resolve exact retained lineage and physical attempt identity before calling those
packages. Generated workers cannot use ordinary pod authority or host execution.

The additional command files separate credential issuance, observation-only
journaling, result surrender, execution monitoring and physical worker recovery.
Their signed HTTP tests also exercise the production factory and actual runner
together. Public child opt-in remains gated pending the complete supported
parent/child journey and Portal qualification; publication and human restart
remain separate landing slices.
