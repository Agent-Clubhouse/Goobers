# Provider cleanup outside the claims lock (#6291)

The command package grows to connect bulk backlog release and terminal cleanup to durable provider-cleanup custody, and to construct trusted daemon-side GitHub, Gitea and ADO cleanup operations. Those adapters depend on command-private instance layout, provider credentials, pinned run repository resolution, lock diagnostics and stage selection; they belong beside the existing assemblies.

Reusable reservation, incarnation comparison and crash/retry behavior live in `internal/localscheduler` and `internal/claimsclient`. The command code only assembles those primitives and keeps provider I/O outside the claims lock. Publication-custody exclusions are part of that boundary, including the legacy key encoding.
