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

### Incomplete reservations

A publish reserves its directory before it writes anything into it, so a
crash, a kill or a lost machine can leave a reservation holding no
`record.json` — typically just `.publish.lock` and `snapshot.bundle.lock`.
Such a directory counts toward `maxSnapshots` like any other entry: unknown
and partial entries are never silently discounted, because capacity has to
reflect what is actually on disk.

An incomplete reservation is reclaimed automatically, and operators do not
delete it by hand:

- For the first hour after its newest file was written it is treated as a
  publish that may still be in flight. An identity-matching retry reuses that
  exact directory and completes it, which is how an interrupted publish
  resumes; until then the reservation is reported and left alone, and it keeps
  its slot.
- After that it is debris. It carries no record, so it has no run, ref or
  repository identity and nothing can be restored from it — not even when it
  holds a bundle, since the bytes cannot be attributed to anything. The
  configured retention pass reclaims it, reporting
  `retention candidate kind=incomplete-recovery-reservation` while the sweep
  is in dry run or inside its first-enable grace window and
  `retention deleted kind=incomplete-recovery-reservation` once enforcing.
- Capacity pressure does not wait for that pass. When a reservation is about
  to be refused for a full inventory, stale incomplete reservations are
  reconciled first — before the landing-proof eviction hook, and independent
  of the retention sweep's dry-run and grace-window gating — and the
  reservation is retried. An instance whose inventory has already filled with
  this debris therefore heals at its first refused cleanup after upgrade, with
  no operator action and no manual quarantine. Reclamation also works while
  the directory holds more entries than the cap, which is the state that makes
  every other reader refuse.

Only Goobers' own debris is reclaimable this way. A directory whose name is
not a reservation identity, or that holds any file other than the known
reservation files, is evidence: it is left in place and keeps its slot until
an operator decides otherwise. A reservation holding a `record.json` is never
touched by reconciliation whatever its state — retirement, expiry and
eviction govern published records.

One incomplete reservation cannot fail unrelated work. A cleanup asking about
its own identity — the abandoned-preparation handoff — reads the inventory
tolerantly and proceeds beside debris it can never match. The strict,
all-or-nothing read is kept for callers asking whether recovery state exists
at all, where an incomplete scan must never be mistaken for absence:
`recovery-abandon`, snapshot selection and viewing, custody pruning, and the
publication API.
