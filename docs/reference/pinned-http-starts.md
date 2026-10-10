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

This is the first LAND-E01 adapter in the
[incremental landing plan](../hitl-advanced-workflows-landing-plan.md).
It covers live-daemon HTTP manual/priority requests. Standalone and detached
starts, targeted PR starts, schedules/demand, signals, direct-engine starts and
future interactive restarts still require their own adapters. This slice adds
neither event ingress nor event publication.

The implementation adapts the ordinary catalog/runtime/start-intent portions of
reference snapshot `fa34a754148ea3076bfd04fe4976a5a461b063f2`. It excludes that
snapshot's event metadata, deadlines, targeted PR delegation and interactive
control dependencies. It uses main's existing configuration compiler, queue
schema and recovery owner; no queue schema migration or baseline reset occurs.

Behavioral validation covers actual HTTP acceptance and execution across a
valid configuration reload, current scope restrictions, concurrent acceptance,
archive lease lifetime, preexisting journal comparison, multi-page retained
generation enumeration and restart reconciliation alongside legacy receipts.
