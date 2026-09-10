# Operating shared claims

Shared claims coordinate participating Goobers instances against a GitHub
repository. Enable them per workflow:

```yaml
spec:
  readiness:
    claimVisibility: shared
```

Omission or `local` retains instance-local admission. Shared mode is GitHub-only;
unsupported providers fail closed. The effective mode and repository are pinned
to the run, so editing current configuration cannot turn an existing shared
lease into a local-only release. GitHub issue selection and execution use the
same routed repository that is recorded in the run's workspace identity.

## Credentials and repository automation

Coordination requires `repo:push` credentials with GitHub Contents write access.
Label reconciliation separately uses `github:issues:write` credentials with
Issues write access. Stage credentials and daemon credentials are resolved at
their respective trust boundaries; a stage token is not accepted as daemon
authority. Missing label credentials produce a reconciliation warning but do
not invalidate a successful ownership transition.

The reserved `goobers-shared-claims/` branch namespace stores append-only
coordination history, not implementation branches. Do not force-update or
delete these refs, and exclude them from generic branch-cleanup automation.
If repository automation runs on every branch push, consider excluding this
namespace to avoid running it on routine lease renewals. Repository rules must
permit non-forced updates by the configured coordination identity.

## Ownership versus labels

Admission requires both the provider lease and the durable local lease. Remote
ownership uses a versioned record and compare-and-swap ref updates. Its owner
binds the instance, run incarnation, and item; reusing a run name does not grant
an old process authority over a successor.

`goobers:claimed` is a human-visible mirror. It is not atomically updated with
ownership, and neither labels nor legacy claim comments decide shared admission.
Labels may lag or temporarily disappear during a takeover race or API failure.
Reconciliation re-reads ownership after label IO and retries remaining drift.
Unrelated labels and synthetic PR/merge-lock/decomposition reservation keys are
not changed by shared issue-label reconciliation.

Provider time determines remote lease expiry. Local execution is bounded by a
conservative acknowledged deadline, reserving one second for HTTP Date precision.
The execution monitor does not extend a known deadline while a claims read is
stalled. Administrative release durably revokes local execution before attempting
remote cleanup, even when provider credentials are unavailable.

Execution snapshots have a one-second read watchdog, including before the first
claim is observed. A stalled read cancels shared execution rather than leaving
it unbounded. Daemon execution snapshots read the atomically replaced ledger
without taking the claim-write lock, so remote renewal does not block them.

## Crash recovery and retry bounds

Before remote issue-claim transitions, the instance writes credential-free
repository registrations under
`scheduler/shared-claim-repo-<repository-digest>.json`. Keep these registrations
with the instance state. They survive normal release and run retention; do not
remove them merely because the local claim ledger is empty.

Acquire, renew, and release attempt immediate label reconciliation with a
two-second bound. An independent daemon loop also runs at startup and once per
minute. Each pass visits at most four registered repositories, each for at most
five seconds and sixteen protocol records. Continuation cursors rotate through
repositories and records; a failed item is retried on a later sweep instead of
blocking other items. Restarting the daemon repeats work from the beginning,
which is safe because reconciliation is idempotent.

There is no fixed convergence-time guarantee during API outages or for large
inventories. A running daemon with the required credentials is needed for
background repair. Removing a repository's credential configuration does not
discard its registration: retries remain visible as errors until access is
restored. Local-only instances without registrations make no retry-provider calls.

Inventory reads are bounded to 4 MiB per repository. Registry enumeration is
bounded to 4,096 scheduler-directory entries, and each registration to 4 KiB.
Exceeding a bound reports an error rather than silently claiming complete
coverage. Periodic failures are recorded in the instance journal under
`shared_claim_visibility_reconciliation_failed`; immediate failures warn that
the label needs reconciliation.

An uncertain release acknowledgment retains the local incarnation for retry.
If another instance has already taken over, a fresh validated read can retire
the old local custody without releasing or changing the successor's record.
Expired orphaned records no longer block admission; the retry scan removes their
stale labels even if the originating run never persisted a local claim.

The cross-instance live-activity query is separate from this feature. Inspect
local claims and the instance journal for lifecycle and recovery evidence; a
visible label alone is not proof of current ownership.
