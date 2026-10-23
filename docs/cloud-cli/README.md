# `goobers-cloud` CLI reference

<!-- Generated from the command registry by `make docs`. Do not edit by hand; edits are overwritten and the CI drift guard will fail. -->

`goobers-cloud` is the cloud control-plane role host. This reference is generated from the CLI command registry, so it always matches the shipped binary.

## Core commands

| Command | Description |
| --- | --- |
| [`goobers-cloud api`](#goobers-cloud-api) | Serve the cloud control-plane API. |
| [`goobers-cloud bootstrap`](#goobers-cloud-bootstrap) | Prepare a new cloud install for cutover. |
| [`goobers-cloud completion`](#goobers-cloud-completion) | Generate a shell completion script. |
| [`goobers-cloud controller`](#goobers-cloud-controller) | Run cloud control-plane reconciliation. |
| [`goobers-cloud cutover`](#goobers-cloud-cutover) | Check cutover readiness of a cloud install. |
| [`goobers-cloud migrate`](#goobers-cloud-migrate) | Apply cloud data-store migrations. |
| [`goobers-cloud worker`](#goobers-cloud-worker) | Run a Temporal engine worker. |

## Advanced operator commands

Less-common commands for configuration, maintenance, and diagnostics.

| Command | Description |
| --- | --- |

## Workflow-stage and connector commands

Runner-invoked workflow internals; these remain directly invocable but are not typically run by hand.

| Command | Description |
| --- | --- |

## `goobers-cloud api`

Serve the cloud control-plane API.

~~~text
Usage: goobers-cloud api [DATABASE OPTIONS]

Serve the cloud control-plane API.

Database options:
  --database-url URL               PgBouncer URL (prefer env/file for credentials)
  --database-url-file PATH         Read PgBouncer URL from a mounted Secret
  --database-direct-url URL        Direct URL (prefer env/file for credentials)
  --database-direct-url-file PATH  Read direct URL from a mounted Secret
  --database-max-conns N           Total connection budget (default 16)
  --database-direct-max-conns N    Direct-pool reservation (default 2)
  --database-auth MODE             password or entra (default password)

Environment:
  GOOBERS_DATABASE_URL         PgBouncer transaction-pool URL
  GOOBERS_DATABASE_DIRECT_URL  Direct PostgreSQL URL
~~~

## `goobers-cloud bootstrap`

Prepare a new cloud install for cutover.

~~~text
Usage: goobers-cloud bootstrap --instance-id ID --backup-ref REF [OPTIONS]

Prepare a new cloud install for cutover: create the database and roles, apply
schema migrations, create the first lease row and object-store bucket, and
leave admissions paused. Every step is idempotent.

Bootstrap refuses before changing anything while any step depends on a backend
that has not shipped; the refusal names each step as "not available until #N".

Options:
  --instance-id ID                 Instance identifier (required)
  --backup-ref REF                 Restorable backup identifier or PITR mark (required)
  --lease-name NAME                Controller lease name (default controller)
  --database-direct-url URL        Direct URL (prefer env/file for credentials)
  --database-direct-url-file PATH  Read direct URL from a mounted Secret
  --database-auth MODE             password or entra (default password)

Environment:
  GOOBERS_DATABASE_DIRECT_URL  Direct PostgreSQL URL
~~~

## `goobers-cloud completion`

Generate a shell completion script.

~~~text
Usage: goobers-cloud completion <bash|zsh|fish>

Generate a shell completion script. Source the output in the target shell.
~~~

## `goobers-cloud controller`

Run cloud control-plane reconciliation.

~~~text
Usage: goobers-cloud controller [DATABASE OPTIONS]

Run cloud control-plane reconciliation.

Database options are the same as for the api role.
~~~

## `goobers-cloud cutover`

Check cutover readiness of a cloud install.

~~~text
Usage: goobers-cloud cutover check --config-dir DIR --instance-config FILE [OPTIONS]

Check that a target install is ready to take over from a local instance.

check runs the cloud-readiness assessment over the instance config (every
workflow must be DSL 3.0+, engine-supported and placeable without the control
plane) and the install checks: schema compatible, lease row present,
admissions paused, object-store bucket reachable. It exits non-zero while
anything blocks; a check whose backend has not shipped reports
"not available until #N" and blocks.

Options:
  --config-dir DIR               Instance config directory (required)
  --instance-config FILE         instance.yaml (required)
  --instance-id ID               Instance identifier (required)
  --lease-name NAME              Controller lease name (default controller)
  --format FORMAT                text or json (default text)
  --database-direct-url URL      Direct URL (prefer env/file for credentials)
  --database-direct-url-file PATH  Read direct URL from a mounted Secret
  --database-auth MODE           password or entra (default password)

Environment:
  GOOBERS_DATABASE_DIRECT_URL  Direct PostgreSQL URL
~~~

## `goobers-cloud migrate`

Apply cloud data-store migrations.

~~~text
Usage: goobers-cloud migrate apply [OPTIONS]

Apply ordered cloud PostgreSQL migrations.

Options:
  --database-direct-url URL        Direct URL (prefer env/file for credentials)
  --database-direct-url-file PATH  Read direct URL from a mounted Secret
  --database-auth MODE             password or entra (default password)
  --backup-ref REF                 Restorable backup identifier or PITR mark (required)
  --instance-id ID                 Goobers instance owning the migration lease (required)
  --holder ID                      Unique migration pod/process identity

Environment:
  GOOBERS_DATABASE_DIRECT_URL  Direct PostgreSQL URL
~~~

## `goobers-cloud worker`

Run a Temporal engine worker.

~~~text
Usage: goobers-cloud worker [OPTIONS]

Run the shared Temporal engine worker host.

Options:
  --temporal-hostport HOST:PORT  Temporal frontend address (default localhost:7233)
  --temporal-namespace NAME      Temporal namespace (default default)
  --task-queue NAME              Task queue to serve (repeatable; default goobers)
  --health-addr ADDRESS          Serve /healthz and /readyz on this address
  --drain-timeout DURATION       Graceful drain bound (default 30s)
  --max-concurrent-activities N  Activity concurrency (default 1000)
~~~

**Examples**

~~~console
$ goobers-cloud worker --task-queue goobers
~~~
