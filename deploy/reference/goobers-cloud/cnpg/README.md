# CloudNativePG reference (goobers-cloud)

The cloud-agnostic PostgreSQL reference for `goobers-cloud` (HA-P3-21, design
decision D-6). Nightly kind runs it for CHAOS-4. Azure installs use the
zone-redundant flexible server in `infra/bicep/modules/postgres.bicep` instead.
Both references declare the same roles and databases, from
`internal/cloud/pgroles`. Tests in that package fail if either reference drifts.

| Resource | Purpose |
|---|---|
| `Cluster` `goobers-pg` | 3 instances, quorum synchronous replication (`method: any`, `number: 1`) with `dataDurability: required`, so acknowledged writes survive a primary loss. PDBs are on (`enablePDB`), and the instances spread across hosts and zones. |
| managed roles | `goobers_owner` (migrate Job, owns the instance database), `goobers_api`, `goobers_controller`, `temporal_app`. All are login roles with no superuser, CREATEROLE or membership. |
| `Database` ×3 | `goobers_example` (rename to `goobers_<instance>`) owned by `goobers_owner`; `temporal` and `temporal_visibility` owned by `temporal_app`. |
| `ObjectStore`, `ScheduledBackup` | The Barman Cloud plugin archives WAL and takes daily base backups. |

## Prerequisites

- The CNPG operator (1.26 or later, for `dataDurability`) and the Barman Cloud plugin.
- Secrets created out of band, never committed:
  - `kubernetes.io/basic-auth` Secrets `goobers-owner-credentials`,
    `goobers-api-credentials`, `goobers-controller-credentials` and
    `temporal-app-credentials`, whose `username` is the role name;
  - `goobers-pg-backup-credentials` with `ACCESS_KEY_ID` and `ACCESS_SECRET_KEY`
    for the object store.
- Replace the `CHANGE-ME` object-store endpoint.

## Roles and grants

After the cluster is ready, `goobers-cloud bootstrap` runs
`pgroles.Provision` as the administrator. It leaves existing roles and
databases unchanged, and it revokes `PUBLIC`'s default `CONNECT` so each role
can reach only its own databases. The schema migrations, run as
`goobers_owner`, then grant table and column privileges from the write-path
inventory.

## Validation

`make deploy-validate` renders this kustomization and checks it with
kubeconform in strict mode. The CNPG schemas are vendored in
[`test/fixtures/crd-schemas`](../../../../test/fixtures/crd-schemas/README.md).
