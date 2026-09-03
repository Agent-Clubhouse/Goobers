# Back up and restore an instance root

This is the restore contract for a Goobers instance root: which paths must
survive, which the product regenerates, which belong to a running process, and
what a safe snapshot of the root actually requires.

It answers a different question than the instance's `.gitignore`. That file
classifies paths into *versioned config* and *generated*, and it is easy to read
as a durability statement. It is not one. `scheduler/` is correctly unversioned
and is also not regenerable: losing it loses which backlog items this instance
believes it owns. **"Not worth versioning" and "safe to lose" are separate
questions**, and only this document answers the second.

For moving a live instance between hosts, follow
[Move a local instance to another machine](move-local-instance.md), which is a
cold migration built on the same classification. For where the root should live
in the first place, see
[Choose where an instance and its config live](instance-placement.md).

## Durability classes

| Class | Meaning |
|---|---|
| **must-survive** | Holds facts no other system can reproduce. A snapshot that omits it is not a restore point. |
| **regenerable** | Derived from must-survive data. Goobers rebuilds it automatically, or a documented command does. Excluding it only costs rebuild time. |
| **transient** | Belongs to a running process. Never restore it; a copy is meaningless or actively misleading on the restored host. |

## The classification

Paths are relative to the instance root.

| Path | Class | Notes |
|---|---|---|
| `instance.yaml` | must-survive | Connections, credential references, and runtime settings. Contains references, never secret values. |
| `config/` | must-survive | The active, materialized definitions. Regenerable *only* when a canonical config source (repository or subtree) is configured and reachable; then materialize from that source instead of restoring this copy. |
| `gaggles/<gaggle>/runs/` | must-survive | Run inputs, journals, checkpoints, artifacts, transcripts, and spans. The authoritative history: the read model and most telemetry are derived from it, and nothing derives it. |
| `gaggles/<gaggle>/workcopies/`, and any configured `workcopies.root` | regenerable | Managed clones and per-run worktrees. Exclude them: they dominate snapshot size, embed absolute paths from the old host, and Goobers clones clean copies on demand. `goobers workspace` recovers pinned workspaces explicitly. |
| `scheduler/claims.json` | must-survive | The claim ledger — the authoritative record of which backlog items this instance has leased. Not derivable from the provider: provider labels are reconciled *from* this ledger, not the other way round. |
| `scheduler/events.jsonl` and `scheduler/events.jsonl.current` | must-survive | The instance journal (scheduler decisions and claim-ledger transitions) and its generation pointer. Restore the pointer with the log it names. |
| `scheduler/blocked.json` | must-survive | The learned blocked-item ledger. Losing it re-attempts items already known to be blocked. |
| `scheduler/docs-updater/`, `scheduler/tutor-holdouts/`, `scheduler/backlog-health/` | must-survive | Durable cross-run cursors: the docs-drift watermark, Tutor live-verification holdouts, and backlog ready-transition ledgers. Each one exists precisely so the next run resumes instead of re-reading a repository's whole history; a holdout has no other copy at all. |
| Other durable stage state under `scheduler/`, such as `post-merge-reconcile.json` | must-survive | Per-stage ledgers and leases that deliberately outlive a run. They are small; restore the whole directory rather than picking files out of it. |
| `scheduler/api-read-cache.json`, `scheduler/api-read-cache.lock*`, `scheduler/sibling-context-cache.json` | regenerable | Bounded provider-response caches and their lock files. Refilled from the provider on demand. |
| `scheduler/up.lock`, `scheduler/claims.lock`, `scheduler/merge.lock`, `scheduler/api.address` | transient | The single-daemon lock and its heartbeat timestamp, cross-process section locks, and the live daemon's API address. They describe a process that is not running on the restored host. |
| `telemetry.db`, `telemetry.db-wal`, `telemetry.db-shm` | must-survive, as one set | The local rollup. Most rows can be rebuilt from run and scheduler journals, but lifetime first-success milestones are carried forward from a readable database and can be lost once retention has removed their source runs. See [Run-journal schema compatibility](schema-migrations.md#telemetry-database). |
| `intake.db` and its `-wal`/`-shm` sidecars | must-survive, as one set | Source watermarks written by out-of-process writers. Deliberately never rebuilt. |
| `read.db`, its `-wal`/`-shm` sidecars, and any `read-<epoch>.db` | regenerable | The Portal's run read model. A daemon that starts without it builds it from journals before serving. One caveat: the retention floor and tombstones are policy state a journal rebuild does not reproduce, so on an instance whose retention has already aged runs out, keep the file to avoid briefly re-admitting expired history. In-progress `read-<epoch>.db` files are rebuild scratch — do not restore them. |
| `blobstore/` | must-survive, when configured | The content-addressed blob store a distributed run's stages exchange artifacts through. Blobs produced on another node are not present in this instance's run directories, so nothing local reconstructs them. A tier 1-2 instance that never served a distributed stage has no such directory. |
| `assets/` and other operator-managed instance content | must-survive | Operator-authored content belongs with the instance. |
| `gaggles/<gaggle>/.journal-backups/` | transient | Pre-migration journal copies, kept only so an interrupted schema migration can roll back. They are rollback scratch for one upgrade, not part of a restore point. |

Anything under the root that is not listed here and not obviously derived should
be treated as **must-survive** until proven otherwise.

Two things are deliberately outside the classification because they are not
instance state at all, and must never be carried in a snapshot:

- **Secret values, harness sign-in state, and OS credential stores.**
  Configuration carries credential *references*; the values belong to the host
  and are re-provisioned there.
- **Service registrations** (systemd, launchd, Windows Service). They embed
  machine-local binary and instance paths; install the service again after
  restoring.

## Taking a snapshot safely

**A hot snapshot is not supported.** Several SQLite databases with WAL sidecars
live in the root, and a snapshot taken mid-write can capture a torn set: the
main file from one instant and its `-wal` from another. Run journals are
append-only and self-repairing, but the claim ledger and the databases are not
covered by that property.

The supported procedure is a cold snapshot:

1. Stop the daemon and wait for the process to exit.

   ```console
   goobers service stop /absolute/path/to/instance   # supervised
   goobers down /absolute/path/to/instance           # foreground `goobers up`
   ```

2. Confirm nothing is still in flight, and keep every Goobers CLI process that
   can write to the instance stopped for the whole copy. This is what makes the
   scheduler journal and each database file plus its sidecars one point-in-time
   set.

   ```console
   goobers runs list --phase=running --json /absolute/path/to/instance
   goobers status --agents --json /absolute/path/to/instance
   ```

3. Copy the root, excluding managed working copies:

   ```sh
   rsync -a \
     --exclude='workcopies/' \
     --exclude='*/workcopies/' \
     "$instance/" "$snapshot/"
   ```

   Leave any external `workcopies.root` behind as well. Copy each `.db` file
   together with its `-wal` and `-shm` sidecars; never copy them in separate
   passes.

4. Store the snapshot as sensitive data. Journals redact known credentials, but
   they still contain source, prompts, logs, and issue content.

If a filesystem or volume snapshot must be taken while the daemon runs — because
the platform offers nothing else — treat the result as a best-effort copy, not a
restore point: it may contain a torn database, and it has no defined
relationship to the provider-side state at that instant. If one must be
restored, run the verification below first, and be prepared to discard
`read.db` (it rebuilds) and to fall back to the last cold snapshot for
`telemetry.db` and `intake.db`.

## Restoring

1. Restore into a stopped instance root. Never start a restored root while the
   original is still running: two daemons with independent claim ledgers claim
   the same provider work twice.
2. Fix machine-local values — absolute `workcopies.root`, outbox mirror,
   script, certificate, and protected-file paths — and re-provision credentials
   and harness sign-in for the account that runs the daemon.
3. Validate before activation:

   ```console
   goobers validate --strict /absolute/path/to/instance
   goobers validate --strict --check-harness --check-repos /absolute/path/to/instance
   goobers runs list --json --limit=50 /absolute/path/to/instance
   ```

4. Start the daemon and confirm the checks under
   [After a restore](#after-a-restore).

### What the daemon repairs on its own

- A torn journal tail from an interrupted write is discarded on open and a
  corrective repair event is appended, for run journals and the instance journal
  alike.
- A missing or unbuilt `read.db` is rebuilt from journals before it is served;
  until then, reads fall back to the journal-derived path.
- Claims whose holding run is already terminal are released at startup, and
  expired leases are swept periodically. A lease is time-bounded, so a stale
  claim does not hold a backlog item forever.
- Runs that were in flight at snapshot time are resumed from their journals.

### What is not reconciled

**There is no reconciliation of provider-side effects against a restored root.**
The instance proceeds from the state it was restored into:

- Effects a run produced after the snapshot — a pushed branch, an opened pull
  request, a posted comment — are not represented in the restored journals. The
  resumed run may repeat them. Inspect in-flight runs before starting, and use
  `goobers run abort` on any run whose external effects you do not want replayed.
- The claim ledger is authoritative for claims, and backlog reconciliation
  corrects provider labels *from* the ledger and current forge state. A restored
  ledger therefore re-asserts its own beliefs about ownership. Review them
  explicitly and force-release anything not genuinely in flight:

  ```console
  goobers claims list /absolute/path/to/instance
  goobers claims list --stale /absolute/path/to/instance
  goobers claims release <item-id> /absolute/path/to/instance
  ```

  `claims release` requires `--force` while the holding run is non-terminal,
  and records the override in the instance journal.

- Durable cursors and watermarks are restored to their snapshot values, so
  scans re-cover the window between the snapshot and the restore. That is
  duplicated work, not lost work.

### After a restore

Confirm each of the following before treating the instance as recovered:

- `goobers status --daemon` reports the expected identity and instance;
- historical runs and scheduler state are visible in `goobers runs list`;
- `goobers telemetry stats` returns without a schema or restore diagnostic;
- `goobers claims list` shows only leases you expect to be live;
- new working copies are created only under the configured roots;
- scheduled and backlog-triggered work resumes once, not from two hosts.

If the telemetry database cannot be opened, restore it together with every
sidecar as one set before considering a rebuild; a partial set is the usual
cause. See [Run-journal schema compatibility](schema-migrations.md).
