# Upgrading workflows from DSL 2.0 to 2.1

DSL 2.1 is an explicit PREVIEW opt-in. DSL 2.0 remains supported; no background
upgrade occurs. The 2.1 watcher uses the durable three-route budget and requires
`comment-on-issue` in addition to the existing `label-issue` action. The same
contract applies to Azure DevOps, GitHub and Gitea. A token's broad scopes do not
substitute for a task's declared policy actions.

Use a binary that supports 2.1 and start with an isolated copy of the instance
configuration. Preserve an unchanged 2.0 baseline for comparison. Preview first:

```sh
goobers fix --to 2.1 ./instance
```

The command shows every workflow diff and a `POLICY ADD` line for each recognized
`goobers pr-comment-watch` task requiring new authority. It retains comments,
user fields and formatting, changing only the version pin and the required
policy list entries. Already-authorized watchers receive only a version edit;
other commands receive no new actions. Unsupported YAML shapes fail without
rewriting the file. Review both the permission additions and the 2.1 behavioral
change before applying them:

```sh
goobers fix --to 2.1 --write --allow-policy-expansion ./instance
```

`--write` alone refuses the entire cohort if any workflow would gain authority.
When no new actions are needed, omit `--allow-policy-expansion`. The flag is an
operator acknowledgment of the displayed additions, not an authorization to
acquire credentials or to change an external service's permissions. This command
does not execute workflows, change instance preview settings, broaden tokens or
modify capability grants. Explicitly configure preview opt-in separately and
validate the resulting configuration with the target binary before starting it.

Every planned file is checked before writing. Mixed unsupported version cohorts
are refused before any workflow writes. Output order is deterministic. Repeating
an already-completed migration is a no-op. The existing direct 2.0 to 3.0 edge is
separate and unchanged; 2.1 does not imply an automatic or implemented 2.1 to 3.0
upgrade.

## Backup and rollback

A write creates one private `.dsl21-migration-backup.json` at the instance root,
outside the scanned `config` directory. It contains the original bytes, modes and
before/after hashes, with a hard bound of 128 files and 8 MiB. There is no growing
backup history. An existing backup is never overwritten. After accepting a
migration, archive or remove this one backup before undertaking a new migration.
Keep it private: like the source configuration, it may contain sensitive values.

To undo the migration, stop the instance's active work first and run:

```sh
goobers fix --rollback ./instance
```

Rollback restores exact original bytes and modes and consumes the backup. It
checks every destination before restoring any file and refuses paths outside
config, symlinks, corrupt backups or files edited after migration. No force flag
can overwrite those user edits. Resolve such a conflict manually while retaining
the backup. A failed/interrupted multi-file write or restore leaves its backup
available; rollback accepts files matching either the original or migrated hash,
so a partially completed operation remains recoverable. A process interruption
may leave `.dsl21-migration.lock`; inspect ownership before removing a stale lock.
