# Durable counted backlog and refill workers

Provider backlog polls and completion-refill observations enter the host's
existing start queue before workers execute. A count describes eligible work;
it does not assign an item or acquire an item claim. The workflow still selects
and claims its own work when its stages run.

## Admission and occupancy

Each observation has a gaggle, workflow, source kind and observation time.
Acceptance retains the configured workflow and Goober digests, configuration
generation, observed eligible count and consecutive worker ordinals. An exact
observation replay reuses its first receipts. Changing its counts conflicts.

A batch contains at most 32 starts. The scheduler subtracts active runs and
already queued starts from the configured capacity. Refill also respects the
workflow's desired occupancy. The queue checks occupancy again inside the same
transaction that accepts all workers; a concurrent manual start or provider poll
cannot allocate the same missing capacity twice.

Accounting includes accepted and uncertain ordinary, signal, schedule and child
starts in the same configured workflow bucket. An exact active run identity is
excluded from the pending count because the scheduler already counts it. An
uncertain dispatch without a live owner remains occupied. Explicit direct-engine
starts keep their existing bypass of local scheduler capacity.

## Execution and recovery

Archive leases span capture through acceptance. Accepted receipts retain their
configuration generations across restart, and repeated provider polls account
for those receipts before adding workers. Execution uses the captured definition
but rechecks current repository scope, enabled source and readiness limits.
Removing the backlog/refill source refuses its pending workers.

An unavailable archive, failed queue commit or changed occupancy does not fall
back to direct execution. The next poll can retry with a new observation. Once
accepted, the same reserved run identity follows the ordinary dispatcher and
journal reconciliation path; missing evidence alone does not authorize repeating
a launch within the same process.

The existing queue's 10,000-record and byte limits apply. Source receipts and
finished starts retain seven days of replay history; unfinished work is retained.
History pressure can therefore refuse new observations even when execution keeps
up. Queue capacity/retention tuning and generic event debounce are separate work.

## Scope and evidence

This LAND-E01 slice covers backlog-count and completion-refill worker starts.
Demand-sized schedule fires still use their prior custody path and need a
separate adapter. Ordinary item claims, provider credential selection and shared
provider-read infrastructure are unchanged by this queue adapter.

The implementation narrowly adapts counted-worker and occupancy parts of
reference snapshot `fa34a754148ea3076bfd04fe4976a5a461b063f2`, without importing
future restart/session/event tables. It extends
[scheduled starts](queued-scheduled-starts.md) and
[signal starts](queued-signal-starts.md).

The acceptance journey uses a local Gitea HTTP fixture, the real daemon setup,
durable queue, archived compiler and local runner: provider poll, acceptance,
host teardown, definition edit, reopen and completion of the original workers.
This proves the queue path with a real provider adapter; it is not a live GitHub,
ADO or Kubernetes qualification.
