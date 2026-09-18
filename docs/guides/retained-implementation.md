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
source run finishes, even when its earlier capture deadline has elapsed. The
sweep also applies the same contentless justifications — `stored-no-diff`,
`bookkeeping-only`, and `superseded-duplicate`, described under
[Reclamation under capacity pressure](#reclamation-under-capacity-pressure)
below — as on-demand reclamation, so the two paths cannot disagree about what
counts as an entry holding nothing worth keeping; a contentless entry is
retired even inside the retain-until floor, and the sweep journals the same
`recovery-reclaimed` annotation the on-demand hook does.

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

### Reclamation under capacity pressure

When a durable handoff is refused because the inventory is full, on-demand
reclamation runs before the cleanup fails. It never chooses a victim: it
retires only an entry it can justify, and records the justification on the
instance log as a `recovery-reclaimed` annotation naming one of:

- `operator-abandoned` — an explicit `goobers recovery-abandon` decision about
  that exact record, on a terminal run. Unlike the periodic sweep, this path
  has no dry-run and no first-enable grace window, so an abandonment frees the
  slot at the moment the capacity is needed rather than a week later.
- `stored-no-diff` — the capture's tree matched its base exactly, so the entry
  holds no patch bytes at all. Captures taken before the empty-diff guard can
  still be present on an existing instance.
- `bookkeeping-only` — the retained patch touches only Goobers' own stage
  outputs (`mutations.jsonl`, `claimed-item.json`, `claimed-items.json` at the
  repository root, and anything under `.goobers/`), never repository content.
  The touched paths are read from the managed mirror; if those objects are
  unavailable the entry is kept.
- `superseded-duplicate` — a newer retained, non-abandoned entry holds
  byte-identical content for the same repository and base, so the identical
  bytes survive the retirement. The newest member of a duplicate set is never
  retired.
- `landing-proven` — the retained work is already present on the target branch,
  proven exactly as the merge-based retirement above requires.

An entry holding a real, unique, unlanded patch is never retired to free a
slot: the cleanup is refused instead, and the worktree is retried. Reservations
a scan cannot interpret are skipped, never retired, and reported on the
instance log as a `recovery_inventory_unreadable` error naming their count and
directories — they consume capacity that no reclamation path can free.

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

Harness-owned stage artifacts never occupy a slot. A live stage cleanup
captures the worktree's tracked changes plus its untracked, non-ignored files,
so Goobers' own throwaway outputs at the workspace root — a stage's declared
`resultFile` (or the provider default for a `goobers` subcommand that carries
one) and the mutation sidecar `mutations.jsonl` — used to be captured as real
content. A one-line write to either made an otherwise-empty patch non-empty,
which defeated the empty-patch guard and consumed a retention-floored slot for
a capture that held no agent-authored work. Those paths are now registered in
the repository's local `info/exclude` before the stage runs, so git omits them
from capture and from `git status`, the patch is genuinely empty, and the
existing guard discards it. A git exclude never applies to a tracked path, so a
repository that legitimately commits a file of one of those names keeps it
visible and keeps it captured. An empty untracked selection adds no paths at
all, as `docs/guides/worktree-retention.md` describes.

`goobers status` reports occupancy as `recovery inventory: <used>/<limit>`
alongside the earliest retention deadline, which is what distinguishes ordinary
pressure from an inventory wedged behind a retain floor. When a cleanup is
refused, the failure names the observed count, the limit, and the root:
`recovery inventory is full: 130 of 128 slots used in /var/lib/goobers/recovery`.
A refused cleanup preserves its source: the worktree stays on disk and is
retried, so a full inventory costs disk and retries, never evidence.

Occupancy is also reported before it fails anything. The daemon samples the
inventory on its own cadence and classifies it against the configured cap:
`healthy` below the high-water mark, `warning` at or above 80% of the cap,
`exhausted` at or above it, and `unavailable` when the reading itself failed —
an unmeasurable inventory is never reported as an empty one. The sample counts
every reservation directory occupying a slot, including incomplete ones that
hold no published record, because those count against the cap until they are
reconciled and so appear in the next refusal's numbers. The limit is resolved
from `instance.yaml` at each sample, so changing `maxSnapshots` is reflected
without restarting the daemon.

That classification appears in three places: the instance read model
(`recoveryInventory` on the instance response, which the portal Overview
renders as a card carrying occupancy, the effective limit, the earliest
retention deadline and elevated styling for warning and exhaustion); the
periodic service-health record in the instance log; and a deduplicated warning
in the instance log. The warning is journalled once when occupancy crosses into
`warning` (`recovery_inventory_high_water`) and once when it crosses into
`exhausted` (`recovery_inventory_exhausted`), never per sample. It is re-armed
only by dropping back below the threshold, and its state is durable, so a
daemon restart with an unchanged condition does not repeat it.

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
