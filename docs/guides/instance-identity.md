# Identify active and historical instance roots

An instance's `.instance-id` is its durable identity, independent of its path,
configuration name, and read-model freshness. Preserve it when moving an
instance. A separately initialized instance receives a different ID. Do not
delete the file to silence duplicate-identity warnings: first determine which
copy owns the continuing history.

```console
goobers status --daemon /path/to/instance
goobers status --json /path/to/instance
goobers roots discover --json /path/to/instances /other/likely/location
```

Normal status, daemon status, and the Portal show the root and its ID. Daemon
ownership comes from the live lock and its recorded owner, not the modification
time of `read.db` or scheduler files. A recorded PID is not necessarily an
owning PID. Unverifiable ownership is reported as uncertainty, never as proof
that the root is authoritative.

Discovery requires only the installed binary. With no paths, it searches the
current directory and user home. Searches are bounded by depth, entry count,
and elapsed time; unreadable or unexamined locations produce a partial result
and exit code 1. Supply explicit likely locations when roots live elsewhere.
Child directory symlinks are not traversed; an explicitly supplied symlink base
is resolved. Discovery does not open a read-model database or adopt a legacy
root merely to inspect it.

Discovery warns about copied IDs and about repositories or gaggle names shared
by roots with owning daemons. Repository comparisons include the provider and
host/project identity. These warnings identify overlap for investigation;
they neither transfer ownership nor provide distributed claim coordination.
See [multiple instances against one repo](multiple-instances-one-repo.md).

## Legacy roots and manual commands

A legacy root without `.instance-id` remains inspectable and is labeled as
unidentified. Startup or a root-mutating command adopts an ID under an exclusive
identity lock. Invalid identity files are not silently replaced.

Manual root mutations print the canonical path and ID before their action.
For initialization, creating the root directory and identity files is the
bootstrap step; the banner precedes configuration and runtime scaffolding.
If the banner cannot be written, scaffolding stops and retry preserves the ID.
Machine-readable command output remains on stdout; identity diagnostics normally
go to stderr. Remote commands query the selected daemon's identity instead of
printing an unrelated local directory. Missing, ambiguous, historical, or
unreadable remote identity prevents submission; upgrade or repair the daemon
before retrying. Redirects cannot silently change the displayed target.

## Mark a retired copy

Stop its daemon and native service first, verify that no active work remains,
and then mark the retired root:

```console
goobers roots decommission --reason="migrated to /new/instance" /old/instance
goobers status /old/instance
```

The command binds `.instance-decommissioned` to the durable ID. It refuses an
active daemon or competing manual lock holder, preserves the first marker on
retry, and deletes no data. Status, discovery, and the Portal report
“historical root; do not use.” Startup and activation commands refuse that
root. Shutdown, service removal, and sensitive-data cleanup remain available;
their diagnostics identify the historical or uncertain target.

For a cold migration, copy the durable identity with the instance, verify the
destination, then mark only the retired source copy. Do not copy that newly
created historical marker into the active destination. A marker is not a
backup, and changing it is not a supported rollback procedure: follow the
[cold migration runbook](move-local-instance.md) to move the authoritative
durable state back. Never run two copies to decide which looks healthier.
