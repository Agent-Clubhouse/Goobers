# Sequential contained parent coordinator (LAND-C06)

The daemon already owns the durable trigger queue, applied and retained source
catalogs, execution leases, shared journal writers, worker transport, cancellation
and repository cleanup. This change connects those existing owners to opted-in
parent stages, with ordinary stages keeping their existing execution policy.
The command additions select and authorize the exact parent attempt, supply the
existing contained worker, and coordinate restart, archive and cleanup with
those process-owned services.

Reusable journal/custody projection, context and budget restoration, independent
Git-state retention, interrupted restore and worktree ownership stay in internal
packages. The command package does not add a parallel scheduler, new authentication
system or separate state store. Recovery and cleanup services are registered in
production composition, and their tests exercise those registered paths.
