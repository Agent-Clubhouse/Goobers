# Move a local instance to another machine

Use this runbook to move one tier 1-2 Goobers instance between workstations,
servers, or small VMs. The procedure preserves workflow definitions, run
history, scheduler and claim state, and telemetry while allowing Goobers to
recreate machine-local working copies.

This is a **cold migration**. Never run the source and destination daemons at
the same time. They would share provider-facing work but have independent local
claim ledgers, which can duplicate claims and external effects.

## What moves

The instance root contains both durable evidence and rebuildable machine-local
data:

| Path | Move it? | Reason |
|---|---:|---|
| `instance.yaml` | Yes | Instance connections, credential references, and runtime settings. |
| `config/` | Yes | The active, materialized definitions. Continue to treat a separate config repository as canonical when one is configured. |
| `gaggles/<gaggle>/runs/` | Yes | Run inputs, journals, checkpoints, artifacts, transcripts, and spans. |
| `scheduler/` | Yes | Scheduling history and the authoritative local claim ledger. |
| `telemetry.db`, `telemetry.db-wal`, `telemetry.db-shm` | Yes, as one set | The local rollup and any SQLite sidecars. Copying all three preserves retained data that may no longer be rebuildable from journals. |
| Other instance-owned files such as `assets/` | Yes | Operator-managed instance content should remain with the instance. |
| `gaggles/<gaggle>/workcopies/` | No | Managed clones and Git worktrees are rebuildable and may contain absolute paths for the old machine. |
| Any external `workcopies.root` | No | Goobers must create clean mirrors and worktrees at the destination path. |
| Service registration | No | systemd, launchd, and Windows Service registrations are machine-local. Install the service again on the destination. |
| Secret values and interactive sign-in state | Re-provision | Goobers configuration should contain credential references, not secret values. Environment variables, protected files, secret stores, and harness sign-in belong to the destination host. |

Do not use this procedure while a run is active. An active run can have
uncommitted or unpushed state in a managed worktree, and this runbook
intentionally does not copy workcopies.

## Before the maintenance window

1. Choose a destination instance root outside every target repository. Prefer a
   short, stable path, especially on Windows. See
   [Choose where an instance and its config live](instance-placement.md).
2. Install the same Goobers version on the destination. A newer compatible
   version may migrate stored schemas on first open; an older binary may reject
   them and must not be used for rollback after migration.
3. Install Git, agent harnesses, workflow toolchains, and any deterministic
   stage executables required by the instance.
4. Re-provision credential sources and harness sign-in for the account that will
   run the destination daemon. Do not copy browser profiles, OS credential
   stores, or secret-bearing home directories.
5. Review host-specific values:
   - absolute `workcopies.root` values;
   - absolute outbox mirror, script, certificate, and protected-file paths;
   - environment-variable names and service environment;
   - webhook listener addresses, DNS, firewall rules, TLS, and OIDC settings;
   - case sensitivity, executable names, and shell assumptions when changing
     operating systems.
6. If the config source is a Git repository, clone or fetch that repository
   separately on the destination. The copied `config/` directory is the active
   materialization, not a replacement for its canonical source.

Record the source version and current state:

```console
goobers version
goobers status --daemon /absolute/path/to/instance
goobers runs list --json --limit=50 /absolute/path/to/instance
goobers status --agents --json /absolute/path/to/instance
```

On PowerShell, use the same commands with a Windows path:

```powershell
goobers version
goobers status --daemon C:\goobers\widget
goobers runs list --json --limit=50 C:\goobers\widget
goobers status --agents --json C:\goobers\widget
```

Resolve failed, escalated, or blocked work according to normal operations.
Wait for running work to complete before starting the cutover.

## 1. Stop the source daemon

For an installed service, stop it through Goobers so the native supervisor does
not restart it:

```console
goobers service stop /absolute/path/to/instance
goobers service status /absolute/path/to/instance
```

For a foreground `goobers up`, request its graceful shutdown from another
terminal:

```console
goobers down /absolute/path/to/instance
```

Wait for the daemon process to exit. A shutdown request being delivered is not
the same as shutdown being complete.

Verify that no run remains active:

```console
goobers runs list --phase=running --json /absolute/path/to/instance
goobers status --agents --json /absolute/path/to/instance
```

If either command reports active work, do not copy the instance. Restart the
source on the same machine, let that work recover and complete, and repeat the
shutdown. Do not solve this by copying its managed worktree.

Keep every Goobers CLI process that can write to the instance stopped for the
remainder of the copy. This is required for a consistent scheduler journal and
for `telemetry.db`, `telemetry.db-wal`, and `telemetry.db-shm` to form one
point-in-time set.

## 2. Copy the instance

Copy into a new, empty staging directory on the destination. Preserve the
source instance unchanged until destination verification succeeds.

Exclude every directory named `workcopies`. For example, on Windows:

```powershell
$source = 'C:\goobers\widget'
$staging = 'D:\goobers\widget.migrating'

New-Item -ItemType Directory -Force -Path $staging | Out-Null
robocopy $source $staging /E /COPY:DAT /DCOPY:DAT /R:2 /W:2 /XD workcopies
if ($LASTEXITCODE -ge 8) {
    throw "robocopy failed with exit code $LASTEXITCODE"
}
```

On Linux or macOS:

```sh
source=$HOME/goobers/instances/widget
staging=/srv/goobers/widget.migrating

mkdir -p "$staging"
rsync -a \
  --exclude='workcopies/' \
  --exclude='*/workcopies/' \
  "$source/" "$staging/"
```

These examples exclude workcopies inside the instance. If
`instance.yaml` or a gaggle points `workcopies.root` somewhere else, leave that
external directory behind too.

Transfer the staging directory through a trusted channel appropriate for the
data. Run journals and artifacts are designed to redact known credentials, but
they can still contain proprietary source, prompts, logs, and issue content.

After transfer, rename the staging directory to the final destination path.
Keep its owner and permissions limited to the daemon account.

## 3. Adapt the destination host

Update only machine-specific configuration. Do not edit scheduler records or
run journals.

1. Change absolute paths that do not exist on the destination.
2. Choose an empty destination for each configured `workcopies.root`.
3. Configure service environment variables and protected credential files.
4. Sign in to each required agent harness as the destination daemon account.
5. Update webhook delivery, DNS, reverse-proxy, TLS, and OIDC configuration if
   the host address changed.

If desired state comes from a checked-in source, make the corresponding changes
there and materialize them through the normal config-delivery path. Avoid
creating destination-only drift by editing only the copied `config/`.

## 4. Validate before activation

Keep the source daemon stopped while validating the destination:

```console
goobers validate --strict /new/instance/path
goobers validate --strict --check-harness --check-repos /new/instance/path
goobers config diff /new/instance/path
```

The first command is local and checks the instance plus its active definitions.
The second also verifies harness readiness, credential resolution, and
authenticated repository access. `config diff` confirms whether the active
materialization matches its configured source.

Inspect the copied history without starting the scheduler:

```console
goobers runs list --json --limit=50 /new/instance/path
goobers telemetry stats --json /new/instance/path
```

Compare recent run IDs and aggregate telemetry with the source snapshot. If the
telemetry database cannot be used, restore the copied database and all copied
sidecars as one set before considering a rebuild. See
[Run-journal schema compatibility](schema-migrations.md#telemetry-database).

## 5. Activate the destination

For foreground operation:

```console
goobers up /new/instance/path
```

For supervised operation, install a new machine-local registration:

```console
goobers service install /new/instance/path
goobers service status /new/instance/path
```

Do not copy or reuse the old service registration. It embeds machine-local
binary and instance paths.

From another terminal, verify:

```console
goobers status --daemon /new/instance/path
goobers runs list --json --limit=50 /new/instance/path
goobers telemetry stats --json /new/instance/path
```

Confirm that:

- the daemon reports the expected identity and instance;
- historical runs and scheduler state are visible;
- no unexpected recovery or schema error appears;
- provider and harness preflights pass;
- new managed workcopies are created only under the configured destination
  roots;
- scheduled and backlog-triggered work resumes once, not from both machines;
- webhook delivery reaches the destination endpoint.

## 6. Finish the cutover

After the destination has operated successfully:

1. Uninstall or permanently disable the source service so a reboot cannot start
   it:

   ```console
   goobers service uninstall /old/instance/path
   ```

2. Keep the source instance snapshot according to the organization's retention
   policy.
3. Remove abandoned source workcopies separately after confirming that no
   recovery is needed.
4. Update operational inventories, monitoring, backup jobs, and ownership
   records with the new host and path.

## Rollback

Before destination activation, rollback is simple: leave the destination
stopped and restart the unchanged source.

After the destination has started, its scheduler and journals may have
advanced. The old snapshot is no longer authoritative and must not simply be
started. Stop the destination, take a new consistent copy of its durable state,
and move that state back using this runbook. Never operate both snapshots to
"see which one works."

If migration started a schema upgrade, use a binary that supports the upgraded
schema. Automatic downgrade is unsupported; see
[Run-journal schema compatibility](schema-migrations.md).

## Cross-operating-system moves

A stopped instance's journals and YAML definitions are portable, but its
workflows may not be. Treat an operating-system change as both a migration and
a placement change:

- do not copy workcopies;
- replace incompatible absolute paths and executables;
- verify shell and script assumptions;
- satisfy every declared OS and toolchain capability;
- run `goobers validate --strict --check-harness --check-repos` before
  activation.

Moving between machines with the same operating system and filesystem
conventions is the lowest-risk path.
