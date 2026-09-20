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

Retention's first-enable grace window (an upgraded instance reports what it
would delete for seven days before it deletes anything, giving an operator
time to review real worktrees and archives) does not hold back a contentless
retirement or an incomplete reservation with no `record.json` at all. Neither
holds anything an operator could review: a contentless entry has no patch
bytes worth keeping by definition, and an incomplete reservation has no
identity to review in the first place. Waiting out the window for these would
turn "an already-wedged instance heals on upgrade" into "it limps for a week,
reclaiming one slot per refused publish" — exactly the outage this exists to
close. Only the operator's own `retention.dryRun` still holds them; every
other retention action (retain-floor expiry, worktree pruning, merged-branch
pruning, and landing-proof retirement) keeps observing the grace window as
before. A pass that deletes either shape while the window is still open says
so in its output, e.g. `retention deleted rule=stored-no-diff kind=recovery
... (grace window does not apply: no recoverable content)`.

Overflow promotion (below) follows that same carve-out for the same reason,
from the other direction: it creates a bundle rather than deleting anything,
so the window has nothing to protect there either.

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
slot: the new capture overflows to the ref tier instead (see
[Inventory capacity](#inventory-capacity)), or, under
`retention.recovery.onFull: refuse`, the cleanup is refused and the worktree
is retried. Reservations
a scan cannot interpret are skipped, never retired, and reported on the
instance log as a `recovery_inventory_unreadable` error naming their count and
directories — they consume capacity that no reclamation path can free.

## Inventory capacity

The recovery inventory is a single instance-wide directory, `<instance
root>/recovery`. Every writer shares it and every writer counts against one
cap: live stage cleanup that contains uncommitted or otherwise unanchored work,
abandoned recovery preparations, the startup crash-orphan worktree reap,
terminal finalization, and archives accepted from remote workers. Ordinary
nonterminal cleanup does not publish recovery when Git proves the worktree is
clean and its current commit is unchanged or anchored by a surviving local
branch. Startup-reap publications are ordinary inventory entries — they are
not exempt from the cap that gates live-run worktree teardown.

Skipping a clean nonterminal cleanup is only safe because terminal
finalization captures the run branch itself. A run whose worktrees were all
removed while it was still running reaches termination with no checkout left
to capture from, and a local branch is not reachable through any part of the
recovery contract. Terminal finalization therefore checks out the run branch's
tip in the managed mirror and publishes it, unless the tip carries nothing the
base does not or some record already retained for that run was captured from
that exact commit. Failure to publish is deferred, not skipped: the run stays
active and finalization is retried.

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
pressure from an inventory wedged behind a retain floor, and appends
`+<n> overflow` when snapshots are being held at the ref tier described below.

A legitimately full inventory no longer refuses the cleanup. Capacity gates
bundle publication, never worktree teardown. A snapshot's objects are written
into the repository - for a linked worktree, the managed mirror - and pinned
at `refs/goobers/recovery-snapshots/<runID>/<sha>` *before* any inventory slot
is consulted, so the scarce resource is the bundle directory, not the work.
Refusing protected nothing and stopped the instance: the cleanup was refused,
the owning run's branch could not be reacquired, and unrelated runs then failed
at `create worktree`.

So when a publish finds the inventory full, it runs the whole drain in order -
reconcile incomplete reservations, reclaim contentless entries, evict landed
ones - and if that still frees nothing it **overflows** instead of refusing.
An overflow entry is one directory under `<instance root>/recovery-overflow`
holding `record.json` alone: the same `Record` schema, the same identity
fields, no bundle and no archive digest. The cleanup is acknowledged, its
`RetainedEvent` carries `recoveryOverflow=true`, and the daemon raises the
exhausted alarm. The tier is uncapped - each entry is a few hundred bytes for
objects that already exist - and it is one durability step below a bundle:
restorable while the managed repository keeps the objects, not self-contained.

The periodic retention pass **promotes** overflow entries back to bundles,
oldest capture first, for as many inventory slots as are free, by republishing
the record through the ordinary publication path against the managed
repository that still holds the pin and then deleting the overflow record.
Promotion honours the operator's own `retention.dryRun` and is never held by
the first-enable grace window - the same split the contentless carve-out uses
above. That window exists so a pass that has not yet earned trust reports what
it would delete before deleting it, and promotion deletes nothing recoverable:
it writes a bundle for objects that already exist and then removes a metadata
record whose identity the bundle now carries. Holding it for a week would
leave an instance's most recent work at the weaker tier for exactly the week
after it was captured. An operator asking for a preview still gets one
(`retention candidate kind=recovery-overflow-promotion ...`). An entry whose
objects are no longer in the managed repository is left in place and reported,
never deleted - the record is the last evidence that the work existed.

`recovery-restore`, `recovery-resume`, `recovery-abandon` and `goobers status`
treat overflow entries as first-class and mark them `overflow`; a restore from
one fetches the pinned ref out of the managed repository instead of unbundling
an archive. Retirement rules - landing proof, explicit abandonment, the retain
floor, the contentless justifications - apply to an overflow entry exactly as
to a retained one, and retiring one unpins the ref and deletes the record.
Nothing is ever discarded for sitting in this tier.

`retention.recovery.onFull` selects the behaviour. The default, `overflow`, is
the above. `refuse` restores the pre-#5370 fail-closed behaviour: the publish
fails with `recovery inventory is full: 130 of 128 slots used in
/var/lib/goobers/recovery`, the cleanup is deferred, and the worktree stays on
disk to be retried, so a full inventory costs disk and retries, never evidence.
Only a genuine capture failure - a tree that cannot be read - still defers a
cleanup under the default, and that blocks one run's branch, never the
instance.

Occupancy is also reported before it fails anything. The daemon samples the
inventory on its own cadence and classifies it against the configured cap:
`healthy` below the high-water mark, `warning` at or above 80% of the cap,
`exhausted` at or above it *or whenever any snapshot is being held at the
overflow tier*, and `unavailable` when the reading itself failed —
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
only by dropping back below the threshold — and, once overflow has been
entered, only after the overflow count returns to zero, so the alarm is not
retracted the moment a single slot frees while work is still waiting at the
ref tier. Its state is durable, so a daemon restart with an unchanged
condition does not repeat it. The read model reports the overflow count as
`recoveryInventory.overflow`, which the portal Overview row renders alongside
occupancy.

An inventory can hold more entries than the configured cap — lowering
`maxSnapshots` on a full inventory produces that state directly, as does the
historical "130 of 128" wedge. It is observed and reclaimed like any other:
the cap is a *write* limit, deciding whether another reservation may be
created, so every reader that exists to reclaim or to observe reads the whole
directory, bounded only by the structural ceiling, and reports the count
against the cap (`goobers status` shows `130/128` and the health sample
classifies it `exhausted`). On-demand reclamation, the retention sweep, the
retirement reaper, incomplete-reservation reconciliation and overflow
promotion all run normally on it, so an over-cap inventory drains on its own.
Raising `retention.recovery.maxSnapshots` is no longer a remedy for it, and
recovery directories are still never deleted by hand.

The strict, cap-bounded read remains in exactly three places, all of them
callers deciding whether it is safe to discard work because recovery state
appears absent: the exact-record checks behind `recovery-abandon`, snapshot
selection for restore and resume, and the publication API. Those return no
partial result and never remove existing records; if one of them is refused for
a full inventory, raise the cap for that operation.

Every other reader reads the whole directory. A caller that only ever touches
its OWN run's records — terminal renewal, the terminal-capture coverage check,
the per-run retained events the read model serves — reads it that way too: a
record it cannot find is nothing to renew or nothing already covered, which is
a no-op rather than a refusal. Bounding those by the cap deferred terminal
finalization of every completed run on an over-cap instance, at every startup,
each deferral holding the worktree and active marker it was trying to release.

Only the cap stopped refusing. A reservation no scan can interpret still does:
`goobers status` reports the inventory as unavailable rather than as absent,
and the guard protecting a run journal from telemetry retention refuses to
prune rather than ruling out ownership it cannot read.

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
