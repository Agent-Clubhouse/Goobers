# Unify local and claims-plane merge exclusion domains (#7705)

This PR adds one daemon-local merge-lock bridge and the surrounding claims-plane
lifecycle wiring in `cmd/goobers`. The bridge makes the instance `merge.lock`
flock reflect durable live `merge-lock/*` claims, restores that exclusion before
the daemon serves claims-plane requests after a restart, and coordinates
nonblocking plane acquire/renew with direct-ledger release, recovery, and
intervention paths. The command package grows by roughly 300 non-test lines and
one file for this assembly.

The reusable primitives remain outside the command package: cross-process file
locking stays in `internal/platform/lock`, and the durable claim ledger plus its
post-persist mutation observer stay in `internal/localscheduler`. What remains
in `cmd/goobers` necessarily binds those primitives to the daemon's concrete
instance layout, claims-lock diagnostics, startup ordering, shared-claim
coordinator, HTTP claims service, and recovery sweep. Moving that wiring into
either reusable package would reverse those dependency boundaries and make the
ledger or lock layer depend on daemon policy.

No new CLI command or public abstraction is introduced. The added code is the
smallest composition layer that ensures local and delegated `merge-pr` windows
share one exclusion domain while preserving per-repository plane concurrency.
