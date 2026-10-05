# Human operations and advanced workflows program

> Status: draft — reviewable design and implementation stack; not a release claim
> Area: workflows, human operations, events, provider reads, portal
> Verified: 04198152b63d228a9714ae2f92a7dca079ba5213 (2026-10-03)

## Purpose and ordering

Give Goobers users a human surface for their software factory while allowing
agents to compose existing capabilities into new, durable work. Deliver in this
order:

1. Agent-authored child workflows: generate, validate, execute, await, and
   reconcile a separate Goobers workflow from an opted-in agentic stage.
2. Human operations: browse interventions, give guidance, repair backlog items
   or PRs through an agent, and restart the affected stage with a new allowance.
3. Durable starts and gaggle events: queue every start, route emitted and
   external events, debounce per consumer, and share provider reads safely.
4. Backlog workbench: browse and edit source-owned work, relationships, and
   objectives across the gaggle's configured repositories and providers.

These priorities order product delivery. Small shared contracts needed by an
earlier slice may land first: child admission needs durable start identity, and
child waits need an intervention representation before the complete portal ships.
This program is compatible with a separate executor/code-duplication refactor;
shared execution entry points should be extended rather than copied into a new
portal or child runner.

## Design map

| Workstream | Design file | Stable task prefix |
|---|---|---|
| Child workflows | `agent-authored-child-workflows.md` | `HAW-CHD` |
| Human operations | `interactive-factory-operations.md` | `HAW-HITL` |
| Events and starts | `gaggle-events-and-durable-start-queues.md` | `HAW-EVT` |
| Backlog workbench | `source-owned-backlog-workbench.md` | `HAW-BKL` |

The filenames above become navigable entries in the generated design index as
those documents enter the stack. Each document owns its acceptance criteria and
unresolved implementation choices. Agreed user decisions below are constraints,
not questions to reopen during implementation.

## Agreed constraints

- Gaggle is the v1 isolation boundary for humans, conversations, children,
  queues, events, credentials, and provider projections.
- Interactive actions use configured gaggle credentials and policy. Log the
  initiating human separately. Optional profile/instructions cannot enlarge
  permissions. Omitted interactive configuration preserves authorized read-only
  monitoring and disables interactive writes and agent sessions.
- Humans can have different permissions in different gaggles.
- Repository changes, including Markdown objectives in a separate wiki,
  workflow, or strategy repository, go through PRs and existing policy.
- Backlog edits may be direct provider actions. An authorized agent can clear
  a resolved `needs-human` blocker after reassessing it; unrelated blockers and
  unmerged repository changes remain effective.
- Human guidance restarts the affected paused stage/loop with prior context and
  a fresh recovery allowance by default. Historical attempts and usage remain.
- Child workflows use existing Goobers and capabilities, have separate workspaces
  forked from current parent work, and cannot recursively create children in v1.
  One unfinished child per stage occurrence permits parallel parent stages to
  each own a child. Canceling a parent cancels unfinished children.
- Parents wait durably, can wait without an active execution timeout, and choose
  merge, replace working state, or discard when the child returns. A child may
  publish PRs only within an upfront parent grant and existing policy.
- Every workflow start becomes queued. Events can match no consumers, one
  consumer, or many. Debounce is configurable independently per consumer.
- Objectives and accepted links are authoritative in the source repository or
  provider. Goobers stores operational history and rebuildable projections, not
  the only copy of organizational relationships. Organization comes before
  progress scoring.

## Current baseline and backlog relationship

This is an additive program, not evidence that earlier issues were incomplete in
their original scope. Existing work supplies pieces rather than the complete
journeys described here.

| Existing area | Reuse | Remaining boundary |
|---|---|---|
| Static parallel stages, typed artifacts, compile/provenance | Native graph execution, result references, pinned definitions | Generated separate child lifecycle and workspace reconciliation |
| Plan-driven fan-out (#1310) | Related roster execution proposal | A fixed template map does not cover a generated child graph |
| Child/subworkflow requests (#155, #817) | Motivation and backlog anchors | Explicit admission, durable wait, publication grant, recovery |
| HITL design and operator messages | Existing intervention and message records | Portal mutations, real continuation execution, gaggle access |
| `internal/triggerqueue` | Durable receipts, bounded SQLite store, uncertain dispatch recovery | All sources, gaggle routing, consumer debounce, leases |
| Provider read cache and quota coordination | Conditional reads, credential fingerprints, call accounting | Shared gaggle polling and ADO query/batch projection |
| Native hierarchy (#6125) | Parent walks, cycle/limit handling | Complete browsing, editable relationships, source-owned objectives |
| Work Items portal page | Recorded operation history | Items without a Goobers run and their associated source graph |

Code inspection is authoritative when issue closure and present behavior differ.
In particular, current `run continue` records a continuation but does not execute
its stages; explicit human gates are rejected by Temporal admission; production
harness nested-agent capabilities do not imply native child workflows; and a
workflow `connectionRef` is not a working runtime credential selector. These are
explicit implementation obligations in the relevant designs.

## Branch and review model

Integration branch: **`codex/hitl-advanced-workflows`**, initially forked from
`main` at the verified revision above. No integration or implementation branch is
merged automatically. A release number can be chosen after team review.

| Sequence | Proposed head | Proposed base | Review unit |
|---|---|---|---|
| D0 | `codex/haw-program-design` | `codex/hitl-advanced-workflows` | This program and task convention |
| D1 | `codex/haw-child-design` | `codex/haw-program-design` | Child lifecycle and admission |
| D2 | `codex/haw-hitl-design` | `codex/haw-child-design` | Human operations and authorization |
| D3 | `codex/haw-events-design` | `codex/haw-hitl-design` | Durable starts, events, shared reads |
| D4 | `codex/haw-backlog-design` | `codex/haw-events-design` | Backlog relationships and workbench |
| I1 onward | `codex/haw-<slice>` | Previous prerequisite slice | Tested implementation acceptance unit |

The documentation branches are stacked because they share a generated index.
Implementation PRs name their exact prerequisites. Avoid stacking unrelated large
refactors into this program; coordinate shared seams with the executor refactor.
Draft PRs may remain incomplete while a required prerequisite is outstanding, but
must state precisely which user journey does and does not work.

### Unnumbered work is intentional

The design task IDs are stable backlog references, usable in commits, tests, and
PR bodies before issues exist. For example, a PR can say “Implements
HAW-CHD-001 from `docs/design/agent-authored-child-workflows.md`.” Do not invent
issue numbers, use fake closing references, or declare a task shipped because its
local branch exists. After the relevant design/item merges, create the agreed
epic and issues, add the mapping below, and backlink the PR and design without
renaming task IDs.

| Stable task | Issue | PR | Delivery evidence |
|---|---|---|---|
| HAW-PGM-001 — design and stack map | Unnumbered | Pending publication | Draft in this document |

Each workstream's task table is the authoritative decomposition until issue
creation. This ledger records publication mappings, not a second backlog.

## Shared implementation rules

- Use one admission and execution path with source-specific adapters. A receipt
  means durable acceptance, not execution or success.
- Every accepted asynchronous action has a durable ID, gaggle, author/source,
  causation chain, pinned policy/config references, and an observable outcome.
- Deduplicate internal submissions by stable caller-supplied action identity.
  Reconcile uncertain external effects before retrying. Never claim exactly-once
  provider writes from a durable internal queue alone.
- Separate human guidance, agent transcript, engine transitions, and provider
  action receipts while joining them by run, stage occurrence, session, and
  causation. Never infer an authoritative state solely from a UI transcript.
- Every accumulating store has named byte/count/time defaults, an actual
  production pruner, and tests proving bounded steady-state behavior. Refuse
  admission when preserving unsettled work would exceed capacity.
- Recheck current authorization on every read/action and before executing queued
  work. Preserve accepted intent and denied outcomes when policy changes.
- Cache keys include authority/credential visibility, provider, resource scope,
  and query shape. Gaggle membership alone does not entitle an interactive
  identity to broader automation-token results.
- New Go types and closed schemas move together, including populated schema
  completeness fixtures. Optional DSL extensions belong in the evolvable
  interpreter; frozen dialects must reject unsupported use.

## Validation and delivery gates

Each implementation slice supplies focused behavior tests and a runnable
production path. State-only helpers without a caller are not a completed feature.
Crash recovery, cancellation races, lost responses, stale revisions, permission
changes, and bounded retention are first-class acceptance cases.

Before publishing a ready implementation PR, run `make verify-fast`; before
merge, the repository's `make ci` contract must pass, including Portal checks
where applicable. Full distributed and provider conformance evidence is required
before enabling a feature on those backends. A local fake test cannot establish
Kubernetes/Temporal or live-provider parity. Record unavailable gates honestly.

## Explicit later scope

Recursive generated children; cross-gaggle events or interactions; full shared
group chat; generated Goober/pod definitions; objective progress scoring; and
more than one unfinished child per stage occurrence remain later work. Preserve
these in the eventual epic instead of silently expanding v1.
