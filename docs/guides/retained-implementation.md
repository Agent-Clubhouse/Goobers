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

## A retained stale worktree after a capture failure

A worktree cleanup that cannot durably hand off recovery fails closed: the
worktree is left in place on disk (never deleted), the run's active marker
stays cleared for retry, and the failure is reported as `worktree cleanup
deferred pending durable handoff: recovery handoff for <id>: ...`. This is
the correct outcome when the underlying git capture itself failed — a
missing ref/object, lock contention, or an unsafe-repository refusal — not
just when the recovery inventory is full.

The wrapped failure carries three pieces of evidence an operator needs, all
preserved in the error chain and in whatever journal/log surfaces it (both
pass through the instance journal's scrubber, so nothing beyond this bounded,
local diagnostic is retained):

- The git subcommand that failed (`merge-base`, `ls-files`, `add`,
  `write-tree`, `commit-tree`, `bundle`, `diff`, ...).
- Its exit code and a bounded (4 KiB) tail of its stderr.
- A failure class: `missing-object`, `locked`, `unsafe-repository`, or
  unclassified when no rule matched.

What to check, by class:

- **`missing-object`** — the repository's object database or a ref this
  capture needed is missing or corrupt. Inspect the worktree's repository
  directly (`git -C <path> fsck`, `git -C <path> rev-parse <ref>`). This
  will not resolve itself on retry; it needs repository repair or, if the
  worktree is unrecoverable, an operator decision to abandon it
  (`goobers recovery-abandon` covers already-retained state — a worktree
  that never captured anything has nothing to abandon and can be inspected
  and removed once its content is confirmed disposable).
- **`locked`** — another git process (or a crashed one's leftover
  `index.lock`) held a lock this capture needed. This is transient: the next
  scheduled cleanup pass retries automatically. If it recurs repeatedly for
  the same worktree, check for a stuck git process against that path before
  assuming the lock file itself is stale.
- **`unsafe-repository`** — git refused to operate on the repository because
  its ownership looks unsafe (a dubious-ownership refusal). This points at a
  host or filesystem-ownership misconfiguration around the worktree root,
  not at the recovery content; it needs host-level investigation, not a
  retry.
- **unclassified** — no rule matched the stderr text. Read the captured
  stderr tail directly; it is the same diagnostic git printed.

A capture failure is distinguishable from a full recovery inventory (see
above) and from a worktree-removal failure (the filesystem refusing to
delete the directory itself, reported separately): each is its own
remediation class, so treating one as another wastes the correct action. In
every case the worktree and its recorded ownership are preserved for retry;
none of these failures is permission to delete recovery evidence or the
worktree by hand.
