# Shared claim caller lock migration (#7598)

The command changes move daemon acquire/release, lifecycle renewal/recovery, backlog selection and administrative cleanup outside enclosing claims locks. These are command-private assembly boundaries: they own trusted provider resolvers, run journals, CLI output, recovery policy and the concrete cross-process lock callbacks. Their coordination must change together to avoid retaining provider I/O under an outer caller lock.

The reusable pending-reservation state machine, exact-incarnation comparisons, shared provider protocol and revocation-before-cleanup sequence live in `internal/localscheduler` and `internal/claimsclient`. The command package adds only integration and response plumbing; obsolete held-ledger constructors are removed.
