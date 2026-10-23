# Cloud cutover runbook

One-time move from a local (or mode 3) Goobers instance to a `goobers-cloud`
install. There is no history import: the old instance keeps its history
read-only for its retention window. Design:
[control-plane-ha.md](../design/control-plane-ha.md#cutover).

The cloud is engine-only. A workflow the engine cannot run is refused at config
apply, so step 0 finds those workflows before you start.

## 0. Readiness

Run both and fix every blocker before continuing:

```sh
goobers validate --target cloud            # on the old instance
goobers-cloud cutover check \              # on the new install
  --config-dir CONFIG_DIR --instance-config instance.yaml --instance-id ID
```

Each blocker names the workflow, stage, DSL path and the unblocking action:
`goobers fix --to 3.0` for `RUNTIME003`, add `runsOn` for `RUNTIME002`, or the
engine-parity issue for `RUNTIME001`. `cutover check` also verifies the schema
is compatible, the lease row exists, admissions are paused and the bucket is
reachable. It exits non-zero while anything blocks. A check whose backend has
not shipped reports `not available until #N` and blocks; do not cut over until
it passes.

## 1. Stand up the new install

Provision Postgres, a fresh HA Temporal and object storage, then:

```sh
goobers-cloud bootstrap --instance-id ID --backup-ref REF \
  --admin-url-file ADMIN_URL --database-direct-url-file OWNER_URL
```

Bootstrap uses two identities: the server administrator creates roles and
databases, and the instance owner runs migrations. The api and controller
roles are created without schema rights; they are the control role's two
pools.

Bootstrap first probes every dependency (database, bucket, roles) without
changing anything, and refuses if any is down. It then creates the database and roles, applies migrations, creates the first
lease row and the bucket, imports local bandit observations (once the
engine supports `task.experiment`, #7763), and leaves admissions paused. Every step is idempotent.
It refuses before changing anything while a step depends on an unshipped backend.

Use one PostgreSQL server per goobers instance. The roles bootstrap creates
(`goobers_owner`, `goobers_api`, `goobers_controller`, `temporal_app`) are
server-wide and are not named for the instance, so a second instance on the
same server would share them. Role provisioning refuses that case with
`ErrRoleDrift` ("owns database outside the contract"). Where the administrator
can act for the maintenance database's owner (self-managed servers), bootstrap
also closes that database to the contract roles. On managed servers the
platform owns it, so it stays open to them; it holds no goobers data and grants
them nothing inside it.

## 2. Quiesce the old instance

Pause admissions on the old instance. Let open runs finish or cancel them
(bounded by the longest run; resolve or abandon human holds). Verify no claims
are held so nothing is double-claimed across instances.

## 3. Switch traffic

Switch the webhook target and trigger clients to the new install, then unpause
it.

## 4. Retire the old instance

Snapshot the old RWO volume and keep it read-only for its retention window to
serve history, then retire it.

## Rollback

Until step 4 retires the old instance, the cutover is reversible:

1. Re-pause admissions on the new install.
2. Repoint the webhook target and trigger clients back to the old instance.
3. Let in-flight runs on the new install finish or cancel them, and verify the
   new install holds no claims, so nothing is double-claimed. This is the
   step 2 invariant run in reverse.
4. Unpause the old instance.

The point of no return is retiring the old instance (step 4): once its volume
is released there is nothing to roll back to, and runs started on the new
install exist only there. Keep the old volume snapshot for its full retention
window. There is no history import in either direction.
