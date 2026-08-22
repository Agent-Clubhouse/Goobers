# Run-journal schema compatibility

Every run journal persists an inspectable `schema.json` with an integer
directory version and the earliest binary that supports that format. This
schema-specific minimum remains stable when later binaries create or migrate a
journal. `schema.json` is the directory schema authority; the `schema` fields in
`run.yaml`, `state.json`, and each event remain payload-envelope versions.

## Compatibility window

Journal schema v1 accepts the pre-manifest v0 layout produced before
`schema.json` was introduced. Its migration adds the manifest without rewriting
`run.yaml`, `events.jsonl`, snapshots, artifacts, or spans. The migration list is
append-only: its zero-based slice index is the source version and index plus one
is the destination version.

An older binary never guesses at a newer format. A directory version above the
binary's supported version, or an unsupported payload-envelope version, fails
closed with the found version, supported version, and minimum-binary
diagnostic. There is no automatic downgrade.

## Atomicity, backup, and interrupted upgrades

`journal.OpenRead` owns forward migration. Before changing a legacy journal,
Goobers waits for its active writer to close and copies the quiescent journal to
the runs root's sibling
`.journal-backups/<run-id>.v<old-version>.bak`. Each migration is deterministic.
Before payload changes begin, `schema.json` atomically switches to an
in-progress marker with version `-1`, the source version, and the target version.
Binaries that do not understand the transaction therefore fail closed rather
than opening a partially migrated journal. The stable target manifest replaces
that marker only after every migration succeeds.

An error before the stable target manifest is visible rolls the journal back
from the backup before releasing its locks. The in-progress marker remains
authoritative until payload restoration is complete. If the target-manifest
rename succeeds but its parent-directory sync reports an error, Goobers does not
roll back beneath that stable manifest: the fully migrated payload stays in
place. After a crash, the atomic rename leaves either the stable target or the
in-progress marker; the latter makes the next compatible open restore the backup
first and retry the migration. The schema migration lock serializes concurrent
openers, while the run writer lock keeps the backup's event ledger and checkpoint
at one consistent writer boundary.

## Rollback

Stop the daemon before restoring a journal. Move the failed run directory aside,
then restore its matching `.bak` directory to the original run ID. The restored
journal requires a binary that supports its recorded version. Automatic
downgrade is intentionally unsupported because run journals are the durable
source of truth.

## Telemetry database

`telemetry.db` records its inspectable integer version in
`schema_meta.version`. The rollup package owns migration when it opens the
database. Its migration list is append-only, and version N means the first N
migrations have committed. Versions below zero and versions newer than the
binary supports fail closed with a restore-or-upgrade diagnostic; there is no
automatic downgrade.

SQLite applies the complete pending migration batch, including each version
update, in one `BEGIN IMMEDIATE` transaction. Concurrent openers serialize on
that write lock. A migration error or process interruption rolls back the whole
batch, so the schema and recorded version remain at the pre-upgrade boundary.
Unlike journal migration, telemetry migration does not create an automatic
backup.

Before upgrading, stop the daemon and every CLI process using the instance,
then back up `telemetry.db` together with any `telemetry.db-wal` and
`telemetry.db-shm` sidecars as one set. To roll back, keep all Goobers processes
stopped, move the upgraded database and sidecars aside, restore that set, and
run a binary that supports its recorded version. Do not point an older binary
at the upgraded database.

Most telemetry rows can instead be rebuilt from run and scheduler journals.
That is not a complete rollback substitute: lifetime first-success milestones
are carried forward from a readable old database because retention may have
removed their source runs. Without a compatible backup, rebuilding after such
retention can lose those lifetime milestones.
