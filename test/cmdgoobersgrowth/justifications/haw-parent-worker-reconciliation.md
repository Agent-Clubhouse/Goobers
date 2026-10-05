# Contained parent worker reconciliation

This slice adds daemon composition for exact retained worker recovery and managed
workspace adoption. The generic transport, bounded recovery receipt, and Git
application replay live in `internal/childpod`; reusable context/workspace restore
lives in `internal/runner`. The command layer owns the daemon registry reservation,
retained configuration lookup, existing workcopy layout, authenticated worker
client, scoped artifact adoption, and lifetime of the exclusive journal writer.
Those dependencies are deliberately not moved into a new generic service package.

The added production command surface is bounded: one serial pending physical
attempt per recovery, the existing four-minute worker teardown bound, a 90-second
custody import bound, and the existing private parent blob quotas. Recovery never
submits a new worker, acquires execution credentials, or consults mutable workflow
names. Tests exercise actual Runner.Start, uncertain worker outcome, recovery,
and human continuation with real Git and a fake authenticated worker boundary.
