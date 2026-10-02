# Scheduled backlog re-sweep

Use the shipped `curate-resweep` workflow alongside ordinary `backlog-curation`.
The former revisits blocked dependencies and ready items; the latter advances
new work toward readiness. The reference and both web example gaggles include
the separate workflow.

The Claude example names it `claude-curate-resweep`, following that example's
prefix convention so combined config-sync output has unique Kubernetes names.

The query stage runs `goobers backlog-query --claim --resweep`, with an explicit
`maxItems` and `resweepMaxItems` (positive and no greater than `maxItems`).
The shipped schedule is `47 4 * * *`, with readiness limits of one concurrent
run, one run per hour, and one run per day. Configure cadence through those
workflow controls, not through query inputs.

## Migration

1. Remove `resweepMaxItems`, `resweepReadyLabel`, and `resweepInterval` from
   ordinary forward-curation query stages.
2. Add the shipped `curate-resweep.yaml` for the appropriate gaggle and curator,
   retaining its claim, dedupe, curate, and release stages and policy grants.
3. Set its schedule/readiness and bounded selection inputs. Do not carry
   `resweepInterval` over: it is rejected even with `--resweep`.

Legacy inline configuration fails with migration guidance instead of silently
running an additional provider scan. The modifier requires `--claim` so the
existing claim-policy authorization remains mandatory.

## Selection and evidence

Forward candidates reserve batch capacity first, but the re-sweep never claims
them. Only remaining slots are available to the two re-sweep lanes, and each
lane has its own budget:

- `resweepDependencyMaxItems` (default 25, at most 250) bounds how many
  `blocked-on-sibling` items have their native blockers rechecked per run. A
  recheck is one cheap provider call, so this budget is sized well above the
  ready-drift lane's. Items whose native blockers have all closed are selected
  as `dependency-recheck` work; an item left unselected only because the batch
  was full is not recorded as swept, so it comes first on the next run.
- `resweepMaxItems` bounds ready-drift reviews, and its presence enables the
  re-sweep. Dependency rechecks no longer draw this budget down, and ready-drift
  reviews never limit how many blocked items are rechecked.

Both lanes still share the run's total `maxItems` batch capacity, and blocked
dependency rechecks are selected before ready-item rechecks. In a run where
more blocked items have become actionable than the batch has free slots, those
items fill the batch and ready-drift review waits for a later run. While that
queue drains, the actionable items stay first in the rotation, so still-blocked
items behind them are rechecked a little later. Partition filters and
priority ordering still apply; shared scheduler-state cursors and bounded
selection history rotate work within priority tiers.

Ready items already in implementation or review are read-only context: no
claim is acquired for them. The result preserves curation mode and staleness
evidence even when metadata reconciliation is disabled.

Dependency/provider failures fail the query stage with a typed error result
and retryability classification. A failed selection does not publish a new
re-sweep cursor/history generation. The separate workflow provides its own
ordinary run and stage journal records; no hidden timestamp determines whether
its query runs.
