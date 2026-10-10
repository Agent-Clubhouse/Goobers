# Pinned HTTP workflow starts

The existing workflow-trigger endpoint durably captures the applied workflow
and resolved Goober definitions when it accepts a new manual or priority start.
An accepted response acknowledges queue custody; execution can wait for normal
capacity. The response format and actor-scoped status endpoint are unchanged.

## Configuration and identity

The daemon records the applied configuration generation, workflow digest and
Goober digest. Editing source files or applying another valid configuration
does not change an already accepted request. An exact retry with the same key,
actor and request returns the original receipt and pins. A changed request or
actor conflicts. Existing legacy receipts remain readable and retain their
previous replay behavior.

At dispatch, the workflow must still exist in the same gaggle and repository.
Current disable, harness, placement, runner capability, readiness and provider
health admission rules still apply. Manual and priority starts retain their
existing distinct provider-health semantics. Captured code cannot expand its
current scope; force does not bypass a disabled workflow.

## Recovery and retention

Acceptance holds an archive lease through the durable queue commit. Queued and
uncertain starts retain their generations even before the startup queue adapter
is installed. A compiled execution holds its lease through the starter's return;
published run journals then supply durable generation ownership.

Scheduler admission alone leaves the receipt in `dispatching`. Only a journal
matching the accepted run identity, generation, digests and trigger confirms
dispatch. An absent journal during a live asynchronous launch does not permit a
second launch. On daemon restart, the existing recovery owner may retry an
unfinished prior-process dispatch after proving journal absence, using the same
reserved run ID. Corrupt or mismatched evidence keeps custody unresolved.

## Incremental scope and source mapping

These are the first LAND-E01 adapters in the
[incremental landing plan](../hitl-advanced-workflows-landing-plan.md).
They cover live-daemon HTTP manual/priority requests and standalone/detached CLI
starts, including standalone targeted PR requests. Same-root file delegation,
schedules/demand, signals, direct-engine starts and future interactive restarts
still require their own adapters. These slices add neither event ingress nor
event publication.

Standalone and detached `goobers run` accept `--request-id`. Retrying the same
request returns its original receipt/run; changed options conflict. The one-shot
owner attempts only that receipt. Capacity-held work remains queued and the CLI
prints its receipt and request ID with a nonzero exit. Retry that ID or start the
daemon to dispatch pending work. Standalone `--no-wait` still waits for admission;
its detached worker owns the instance lock and cleanup until execution stops.
The existing prohibition on standalone duration-limited runs remains in force.

Targeted PR validation checks both captured and current signal subscriptions
and the current configured provider identity before dispatch. A closed or
inaccessible PR is refused without launching. Provider validation is bounded
separately from the execution lifetime. An unavailable queue never falls back to
an unrecorded launch.

The implementation adapts the ordinary catalog/runtime/start-intent portions of
reference snapshot `fa34a754148ea3076bfd04fe4976a5a461b063f2`. It excludes that
snapshot's event metadata, deadlines, same-root file delegation and interactive
control dependencies. It uses main's existing configuration compiler, queue
schema and recovery owner; no queue schema migration or baseline reset occurs.

Behavioral validation covers actual HTTP acceptance and execution across a
valid configuration reload, current scope restrictions, concurrent acceptance,
archive lease lifetime, preexisting journal comparison, multi-page retained
generation enumeration and restart reconciliation alongside legacy receipts.
