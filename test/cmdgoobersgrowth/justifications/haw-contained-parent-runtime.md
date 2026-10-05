# HAW contained parent runtime composition

The command layer composes the daemon's existing source-generation leases,
credential authority, worktree ownership, scheduler and authenticated Temporal
transport into one parent executor factory. These host dependencies already
live in cmd/goobers. The generic bounded carrier, private checkout, exact pod
termination, returned-tree import and worker dispatch remain in internal/childpod,
internal/recovery, internal/dispatcher and internal/engine. No new CLI command,
Kubernetes client or daemon Kubernetes permission is introduced.

The first supported parent shape uses serial agentic repository tasks and
model-only credentials in Linux image runners. Unsupported topology and
unresolved physical custody refuse execution explicitly. The host factory also
binds retained kits and recovery markers to the actual journal stage sequence,
so source lookup, retry and human replacement cannot bypass the same boundary.
