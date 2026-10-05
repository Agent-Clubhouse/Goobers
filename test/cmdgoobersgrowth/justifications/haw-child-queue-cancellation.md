# HAW-EVT-007: exact initial-child cancellation adapter

The daemon adapter connects current human queue authorization to its existing
child launcher, runner ownership registry, retained child journals, and contained
worker reconciliation. These host-only dependencies cannot live in the storage
or transport packages. The cancellation transaction and initial-execution
barriers remain in `internal/triggerqueue`; the shared policy and response
semantics remain in `internal/startcontrol`. The added host observation path
neither resumes a child nor reacquires human policy, preventing authority-lock
reentry and preserving later human execution epochs.
