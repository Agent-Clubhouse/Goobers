# LAND-E01: shared-directory file delegation

The command package owns the existing request-file protocol, claim/withdrawal
operations and daemon sweep. This adapter hands that protocol to the existing
start-intent service using a stable file identity, then delivers the durable
receipt through the existing response files. File custody and immutable queue
custody must be reconciled before stale-file rules or withdrawal can act.

Deadline persistence and journal-aware expiry live in internal/startintent;
there is no second queue, credential resolver or SQL migration. Command tests
exercise the actual scheduler setup, accepted archives, daemon drain and file
recovery alongside the existing file-protocol regression suite. This declaration
covers only the file adapter and does not change a growth baseline.
