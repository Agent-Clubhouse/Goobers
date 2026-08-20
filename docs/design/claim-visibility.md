# Design: Claim visibility - local by default, shared by opt-in

> Status: **approved — decision record; contract only, not implemented**
> Requirements: [`docs/requirements/scheduler.md`](../requirements/scheduler.md)
> (`SCH-020`-`SCH-022`),
> [`docs/requirements/backlog-providers.md`](../requirements/backlog-providers.md)
> (`BL-005`, `BL-032`, `BL-033`)
> Architecture: [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md) sections 4, 6, and 7
> Follow-up implementation: [#1487](https://github.com/Agent-Clubhouse/Goobers/issues/1487)
> Live activity query: [#1488](https://github.com/Agent-Clubhouse/Goobers/issues/1488)

## Decision

At tiers 1-2, each instance's local `ClaimLedger` remains the default source of
truth for claim ownership and lease lifecycle. Its claim key is scoped by gaggle,
provider, and external item ID. A provider-visible marker in this mode is a
human-readable mirror: a missing or stale marker does not grant or transfer a
local lease.

Workflows may explicitly request cross-instance claim visibility:

```yaml
spec:
  readiness:
    claimVisibility: shared
```

`claimVisibility` has two modes:

| Value | Claim scope and authority | Provider state |
|---|---|---|
| `local` | Default. The per-instance `ClaimLedger` is authoritative within that instance. | A non-authoritative mirror may be written for human visibility. |
| `shared` | Opt-in. The local ledger remains authoritative for the instance's run ownership and lease, while a provider-backed claim record is an additional admission constraint shared by participating instances. | A shared coordination record, distinguishable from a local mirror, must be acquired and released as part of the claim lifecycle. |

Omitting the field is exactly equivalent to `local`. Upgrades, provider changes,
and new query surfaces must not silently turn a local workflow into a shared one.
An unsupported provider must reject `shared` rather than fall back to local
visibility.

### Provider record roles

Provider state written for `local` and `shared` modes has different semantics and
must be unambiguously distinguishable:

- A **local mirror** reports one instance's local-ledger state for humans. It is
  not a shared claim, even when it refers to the same item or resembles a claim
  marker.
- A **shared coordination record** identifies itself as participating in the
  `shared` protocol and carries the identity needed for owner-scoped acquisition,
  renewal, and release.

Shared admission and live-activity queries must consider only shared coordination
records and ignore local mirrors. Conversely, local-marker reconciliation and
cleanup must not mutate or remove shared coordination records. The concrete
GitHub encoding - for example, a separate marker namespace or an explicit mode
discriminator - belongs to #1487; the required behavioral separation does not.

## Claim and release contract

Both modes preserve the existing lease rules:

- A run must acquire its claim before processing the item. A live claim held by
  another run blocks admission; an idempotent reacquisition by the owning run may
  renew its lease.
- The owning run releases the claim as soon as it no longer needs the item,
  including normal completion and terminal failure. Crash recovery and lease
  expiry release orphaned claims. Administrative force-release remains an
  explicit, journaled recovery action.
- Claim and release transitions remain journaled. Normal release is owner-scoped
  so one run cannot release another run's claim.

In `local` mode, provider-marker writes and cleanup remain mirrors. A provider
mutation failure does not replace or invalidate the ledger decision; reconciliation
may repair the visible marker later.

In `shared` mode, admission succeeds only when the local lease and the remote
claim record have both been established. A remote record held by another
instance blocks the claim. Failure partway through acquisition must not start
work with a partially established claim; the implementation must roll back or
reconcile the partial state and fail closed. Normal completion, terminal
failure, crash recovery, and lease expiry must remove the remote record alongside
the local release. Failed remote cleanup must remain visible for reconciliation
rather than being reported as a complete shared release.

## Provider sequence

The first `shared` implementation is GitHub-specific and is tracked by
[#1487](https://github.com/Agent-Clubhouse/Goobers/issues/1487). It encodes a
GitHub shared coordination record distinct from the local human-readable mirror
while retaining the local ledger's owner and lease data.

Azure DevOps must not accept `claimVisibility: shared` until the provider reaches
claim-marker parity through [#32](https://github.com/Agent-Clubhouse/Goobers/issues/32).
After that parity exists, the same workflow-level contract applies without a
provider-specific definition shape.

## Live activity visibility

Local workflows remain inspectable through their instance ledger and scheduler
journal. A cross-instance "what is active now" view cannot infer authoritative
global activity from those isolated ledgers.

The portal/CLI live query tracked by
[#1488](https://github.com/Agent-Clubhouse/Goobers/issues/1488) therefore builds
on provider-backed records from `shared` mode. It is a read surface over shared
claim state, not a reason to change the default, and it must not present local
workflows as globally visible.

## Scope

This record reserves the workflow contract and its semantics only. It does not
add the DSL field, provider mutations, reconciliation, or the live query surface;
those belong to #1487 and #1488.
