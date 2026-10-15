# HAW-EVT-008: stage provider read scope

The existing command-package provider constructor adds only the translation from
trusted stage environment fields into the reusable cache scope, and uses that
scope for snapshot invalidation. Gaggle and pinned configuration generation are
process-launch facts owned by this wiring. Partitioning, HTTP handling, size
bounds and SQLite reuse remain in internal/apireadcache. No new command or polling
loop is introduced. A production-constructor test covers scope and invalidation.
