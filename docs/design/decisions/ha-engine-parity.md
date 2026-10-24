# HA engine-parity decision

- Status: approved
- Date: 2026-10-21
- Decision owner: issue #7751
- Programme: control-plane HA, phase P4

## Context

The Temporal engine refuses eight DSL feature categories that the local runner
supports: static parallels, parallel selection, maps, child workflows and their
parents, human gates, remote token and cost limits, task outbox export, and
experiments. Decision 005 R9 described these behaviors as not being ported.

That position is incompatible with the approved cloud runtime. The cloud control
plane is engine-only and never falls back to executing a stage on its own host.
Leaving any category runner-only would therefore make a locally valid workflow
ineligible for cloud execution.

The draft [child-workflow design](../engine-child-workflows.md) already proposed
reversing R9 for child workflows, parallels, selection, and maps, but put child
workflows before fan-out. Shipped reference and example workflows use parallels
and selection today; none currently declares the other refused categories.

## Decision

Reverse decision 005 R9 for all eight engine-refused feature categories. Each
category will be ported to the engine during P4. Its existing refusal remains in
place until the complete implementation, recovery coverage, replay protection,
runner-to-engine parity evidence, and runtime-support registry update land.

Implement parity in this usage-led order:

1. static parallels;
2. parallel selection;
3. maps;
4. child workflows and child-workflow parents;
5. human gates;
6. remote token and cost limits;
7. task outbox export;
8. experiments.

This order amends the child-workflow draft: fan-out no longer waits for child
workflows. Static parallels use concurrent engine-workflow coroutines and a
deterministic join; selection freezes its result in history; maps freeze their
roster by digest and use bounded fan-out. Child workflows retain the draft's
frozen proposal, durable origin, Temporal child execution, and parent-custody
model.

Human gates use an occurrence-bound Temporal Update and durable gate journal
events. Outbox export is an additive surrender manifest of content-addressed
artifacts. Experiment assignment is replay-stable in history, and observations
use a closed plane route to a durable store. These mechanisms are specified in
the [engine-parity design](../control-plane-ha.md#engine-parity).

For remote limits, the trusted stage-pod supervisor (`__dispatch-exec`) reads
usage from the harness adapter, enforces the in-stage cap, and reports usage in
an additive surrender field. The engine walk applies cumulative charging.
Supervisor-measured usage is trusted to the same degree as usage measured by
the local runner: it is trusted product-runtime evidence, not a value supplied
by workflow-authored code. Protocol evolution remains versioned and tolerant
of the current and immediately preceding contract versions.

## Consequences

- Cloud remains engine-only. No runner or control-plane-host execution fallback
  is introduced while parity work is incomplete.
- Workflows using an unported category remain explicitly ineligible for cloud
  execution; local-runner behavior is unchanged.
- Parallels and selection unblock the currently shipped examples first. Later
  items reuse their deterministic fan-out machinery where applicable.
- The engine implementation must preserve runner-visible semantics while adding
  replay, worker-loss recovery, and upgrade compatibility.
- The DSL 3.0 stable lock does not block implementation, but the final
  qualification sweep follows the lock and must absorb any semantic changes.

Closes #7751.
