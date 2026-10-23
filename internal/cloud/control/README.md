# Cloud control role (HA-P3-18)

`goobers-cloud control` combines the authenticated HTTP planes with a
single-active controller. It does not register a Temporal worker or execute a
stage. The remote engine task queue is served independently.

The lease manager renews every 5 seconds, expires after 15 seconds, and fences
locally after 10 seconds without a successful renewal. Standbys retry every
2 seconds. Schema compatibility is checked before acquisition and while active.
Every epoch rebuilds claims against open workflows, reconciles uncertain starts,
and reattaches awaiters before readiness or any L1/L2/L3 tick. Losing authority
cancels the epoch. SIGTERM cancels loops, stops epoch services, and voluntarily
releases the lease; it does not cancel Temporal workflows.

BeginDispatch is the provisional reservation. Starts use only the durable row's
run ID and are confirmed/adopted after Temporal classification. A new owner gets
one immediate attempt for predecessor dispatches; unresolved attempts retain
backoff and remain visible as `start ownership uncertain`. Attempts are bounded
and independent, so one uncertain workflow cannot stop healthy admission.
Provider observations are bounded JSON snapshots, restored before rebuilding
and fenced before admission. Provider adapters supply versioned, non-secret
quota/MaxOpenPRs observations; snapshots never authorize execution themselves.

## Required composition seams

The command's `assembleControlPlanes` must receive the independently landing PG
read, claims, trigger, journal-write and journal-read services, pinned generation
resolver, and epoch service factory. The factory supplies rebuild-before-reap,
uncertain reconciliation, await reattachment, policy/renewal/config ticks,
provider observation capture/restore and shutdown. Each mutable backend must
bind the exact acquired handle; it must not borrow a newer global owner from an
old goroutine. Callbacks must honor cancellation. A failed builder returns any
allocated Stop callback so the host can clean up.

The assembly also receives signed pod/grant minting keys, credential-grant
verification and durable revocation. Key reload runs for the process lifetime;
malformed replacement key rings preserve the last good keys and report errors.
The default human authenticator denies access pending the cloud identity
adapter. Status requires an instance-view principal; pod/grant identities cannot
read global status. Health/readiness probes carry no private workflow data.

Until those PG/lifecycle seams are assembled, the command serves probes and
status with readiness false. There is no local filesystem, SQLite, registry-auth
or in-process runner fallback, and no fabricated successful recovery callback.
The actual dependency assembly is deliberately reviewable in the command.

## Validation

`GOOBERS_TEST_POSTGRES_URL` points only to a local PG16 container. Run:

```
go test -race -tags cloudintegration ./internal/cloud/control ./internal/cloud/acceptance
```

The control suite uses a real Temporal dev server with no worker, simulates a
lost response after Temporal accepts a start, transfers the lease between two
replicas, verifies one start and restored observations, and reports RESTART-RTO.
It also proves a released/taken-over owner stops within the self-fence bound.
A repeated nightly environment is still needed for a meaningful p95 RTO result;
a single test's elapsed time is not a production percentile.
