# Partition-aware backlog health

The scheduled curation workflow's `sample-ready-pool` stage runs
`goobers backlog-health`. Its `requireLabels` input inherits the gaggle's
partition unless the stage explicitly overrides it. The ready-pool depth counts
open, unclaimed ready items inside that scope; sibling and unpartitioned ready
items cannot make an empty partition appear healthy.

An observed empty pool sets `readyPoolStarved: true` and emits a
`WARNING: ready pool starved` line in the stage output. The existing telemetry
rollup consumes measured depth and the Insights page displays `0 · Starved`.
A deferred transition scan has no observation timestamp and emits no starvation
warning or depth sample. These are operational diagnostics, not a reason to
claim parked work or relax its approval gates.

For a backlog shared by label-based partitions, add the complete, operator-owned
partition vocabulary to the health stage:

```yaml
inputs:
  trustLabel: "goobers:approved"
  readyLabel: "goobers:ready"
  # requireLabels still inherits this gaggle's own partition.
  partitionLabels: "goobers:cloud,goobers:local"
  resultFile: "backlog-health.json"
```

When `partitionLabels` is configured, the read-only diagnostic performs an
additional paginated query for open items with `trustLabel` in the backlog
repository. `unpartitionedApprovedCount` counts those with **none** of the listed
partition labels, whether ready or not. Closed, unapproved, and valid sibling
items do not contribute. A nonzero count emits a warning asking the operator to
choose a partition. Query failure fails the stage instead of publishing zero.

The report records `requireLabels` and `partitionLabels` beside the measurements.
Without an explicit partition vocabulary, `unpartitionedApprovedCount` is
omitted rather than guessing which sibling labels exist. A configured vocabulary
requires `trustLabel`; include every partition membership label and update it
when partitions change. This count diagnoses simple label membership, not
arbitrary composite partition predicates or assignment restrictions.

Health sampling never adds partition labels, clears parks, or claims items.
The optional population diagnostic does not change `--feedback` selection.
