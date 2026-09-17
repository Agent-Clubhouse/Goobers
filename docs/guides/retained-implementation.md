# Retained implementation recovery

Recovery preserves an implementation independently of its original worktree.
The retained record binds the source run and repository to a base commit,
snapshot commit, patch digest, independently verified bundle, and retention
deadline. Removing a worktree is not permission to discard this archive.

Use [`goobers recovery-restore`](../cli/README.md#goobers-recovery-restore) to
create a new operator branch on freshly fetched main. The
[`goobers recovery-resume`](../cli/README.md#goobers-recovery-resume) workflow
stage adopts a verified restoration into a receiving run. Conflicts, expired
archives, and changes to protected runtime assets refuse restoration.

## Retirement

The retention sweep requires the source run to be terminal and its journal
writer idle. It can retire an archive after explicit, trusted operator
abandonment, after the bounded retention window, or after verified landing of
its restored content. The deadline path also preserves thirty days after the
source run finishes, even when its earlier capture deadline has elapsed.

Early merge-based retirement requires all of the following:

- A terminal, idle receiving run in the source run's configured run directory,
  with the same immutable repository identity.
- A persisted landing intent and positive merge confirmation from that same
  run, paired by intent ID, repository API address, and pull request ID. Both
  the requested head SHA and returned merge SHA must be available.
- An exact source-bound restoration in the receiving head's history. Its tree
  must match a replay of the retained patch onto its parent.
- Unchanged retained paths in both the receiving head and actual merge commit.
  Squash merges are supported; subsequent reverts do not satisfy this check.

Completion status, queue admission, branch names, and copied commit messages
are not merge proof. Legacy restorations without a source identifier, manual
merges without paired journal receipts, unavailable objects, or conflicting
evidence retain their archives until another retirement rule applies. The
evidence scan is bounded to 256 directory entries and 16 MiB per receiving
journal; exceeding a bound reports a retention error, not successful cleanup.

Verification checks the existing managed mirror and pinned clone under their
custody locks. A complete proof in either copy suffices, but removal checks
every owned recovery ref before retiring the archive. Dry-run retention reports
candidates without deleting archives. Main and operator branches are not
recovery refs and are not deleted by retirement.

Use [`goobers recovery-abandon`](../cli/README.md#goobers-recovery-abandon) only
when the retained implementation is deliberately no longer needed. The command
requires the exact source run, recovery ref, and patch digest; its durable
operator annotation authorizes the later sweep.

## Inventory capacity

The recovery inventory is a single instance-wide directory, `<instance
root>/recovery`. Every writer shares it and every writer counts against one
cap: each gaggle's live stage cleanup, the startup crash-orphan worktree reap,
terminal finalization, and archives accepted from remote workers. Startup-reap
publications are ordinary inventory entries — they are not exempt from the cap
that gates live-run worktree teardown.

`retention.recovery.maxSnapshots` in `instance.yaml` sets that cap (128 when
the section is omitted). It is resolved from `instance.yaml` at the point of
use rather than from whatever configuration a caller was built with, so a
raised cap applies to live cleanups without a daemon restart and no path can
enforce a different limit than the one `goobers status` reports for the same
directory. A resolution that cannot read `instance.yaml`, and so falls back to
a carried configuration or the built-in defaults, journals a
`recovery_policy_fallback` error naming the limit it is about to enforce and
the inventory root it applies to.

`goobers status` reports occupancy as `recovery inventory: <used>/<limit>`
alongside the earliest retention deadline, which is what distinguishes ordinary
pressure from an inventory wedged behind a retain floor. When a cleanup is
refused, the failure names the observed count, the limit, and the root:
`recovery inventory is full: 130 of 128 slots used in /var/lib/goobers/recovery`.
A refused cleanup preserves its source: the worktree stays on disk and is
retried, so a full inventory costs disk and retries, never evidence.

If the inventory already contains more entries than the configured cap, do
not delete recovery directories by hand. Temporarily raise
`retention.recovery.maxSnapshots` above the current inventory size, retry the
inspection or exact `recovery-abandon` operation, and let configured retention
remove only records whose ownership and retention checks succeed. Inventory
overflow returns no partial result and never removes existing records.
