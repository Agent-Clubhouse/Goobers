# Shared session native runtime

This change adds the daemon composition for durable, human-authorized session
turns. The command package selects an archived Goober, installs the existing
interactive credential/runtime lease, connects scheduler and queue ownership,
observes actual journal and writer custody, and wires the HTTP session service.
These adapters depend on daemon registries and lifecycle and therefore remain
in cmd/goobers. Conversation records, authorization, scheduling, execution inputs,
and journal evidence stay in their existing internal packages. The shared human
runtime setup is extracted from the existing restart adapter to avoid copying its
credential and workspace-quiescence lifecycle.

The seven new production files separate profile selection, execution, observation,
settlement, daemon installation, retention, and the shared runtime entrypoint.
No shared growth snapshot is re-pinned.
