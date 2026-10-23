# `goobers-cloud` CLI reference

<!-- Generated from the command registry by `make docs`. Do not edit by hand; edits are overwritten and the CI drift guard will fail. -->

`goobers-cloud` is the cloud control-plane role host. This reference is generated from the CLI command registry, so it always matches the shipped binary.

## Core commands

| Command | Description |
| --- | --- |
| [`goobers-cloud api`](#goobers-cloud-api) | Serve the cloud control-plane API. |
| [`goobers-cloud approve`](#goobers-cloud-approve) | Approve a paused stage through the API. |
| [`goobers-cloud cancel`](#goobers-cloud-cancel) | Cancel a live run through the API. |
| [`goobers-cloud claims list`](#goobers-cloud-claims-list) | List active claims through the API. |
| [`goobers-cloud claims release`](#goobers-cloud-claims-release) | Release a cloud claim. |
| [`goobers-cloud completion`](#goobers-cloud-completion) | Generate a shell completion script. |
| [`goobers-cloud config apply`](#goobers-cloud-config-apply) | Apply cloud configuration. |
| [`goobers-cloud config status`](#goobers-cloud-config-status) | Show cloud configuration status. |
| [`goobers-cloud controller`](#goobers-cloud-controller) | Run cloud control-plane reconciliation. |
| [`goobers-cloud deny`](#goobers-cloud-deny) | Deny an escalated run through the API. |
| [`goobers-cloud lease status`](#goobers-cloud-lease-status) | Show cloud controller lease status. |
| [`goobers-cloud migrate`](#goobers-cloud-migrate) | Apply cloud data-store migrations. |
| [`goobers-cloud migrate status`](#goobers-cloud-migrate-status) | Show cloud schema migration status. |
| [`goobers-cloud override`](#goobers-cloud-override) | Override a paused stage through the API. |
| [`goobers-cloud runs list`](#goobers-cloud-runs-list) | List runs through the API. |
| [`goobers-cloud runs logs`](#goobers-cloud-runs-logs) | Read one run's journal events through the API. |
| [`goobers-cloud runs show`](#goobers-cloud-runs-show) | Show one run through the API. |
| [`goobers-cloud status`](#goobers-cloud-status) | Show cloud instance status through the API. |
| [`goobers-cloud triggers`](#goobers-cloud-triggers) | Submit a durable workflow trigger through the API. |
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

## `goobers-cloud approve`

Approve a paused stage through the API.

~~~text
Usage: goobers-cloud approve --api URL --idempotency-key KEY [--actor NAME] [--rationale TEXT] RUN STAGE

Approve one paused or escalated stage through the control-plane API.
~~~

## `goobers-cloud cancel`

Cancel a live run through the API.

~~~text
Usage: goobers-cloud cancel --api URL --idempotency-key KEY [--actor NAME] [--gaggle NAME] [--workflow NAME] RUN

Cancel one live cloud run through the control-plane API.
~~~

## `goobers-cloud claims list`

List active claims through the API.

~~~text
Usage: goobers-cloud claims list --api URL

List active cloud claims through the control-plane API.
~~~

## `goobers-cloud claims release`

Release a cloud claim.

~~~text
Usage: goobers-cloud claims release [OPTIONS]

Release a cloud claim. This command is reserved until the PostgreSQL claims
backend supplies an operator-safe release operation.
~~~

## `goobers-cloud completion`

Generate a shell completion script.

~~~text
Usage: goobers-cloud completion <bash|zsh|fish>

Generate a shell completion script. Source the output in the target shell.
~~~

## `goobers-cloud config apply`

Apply cloud configuration.

~~~text
Usage: goobers-cloud config apply [OPTIONS]

Apply a cloud configuration generation. Reserved until #7827.
~~~

## `goobers-cloud config status`

Show cloud configuration status.

~~~text
Usage: goobers-cloud config status [OPTIONS]

Show cloud configuration generation and eligibility status. Reserved until #7827.
~~~

## `goobers-cloud controller`

Run cloud control-plane reconciliation.

~~~text
Usage: goobers-cloud controller [DATABASE OPTIONS]

Run cloud control-plane reconciliation.

Database options are the same as for the api role.
~~~

## `goobers-cloud deny`

Deny an escalated run through the API.

~~~text
Usage: goobers-cloud deny --api URL --idempotency-key KEY --rationale TEXT [--actor NAME] RUN

Deny one escalated run through the control-plane API.
~~~

## `goobers-cloud lease status`

Show cloud controller lease status.

~~~text
Usage: goobers-cloud lease status [OPTIONS]

Show the active cloud controller lease and fence epoch. Reserved until #7803.
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

## `goobers-cloud migrate status`

Show cloud schema migration status.

~~~text
Usage: goobers-cloud migrate status [OPTIONS]

Show cloud schema migration compatibility. Reserved until #7802.
~~~

## `goobers-cloud override`

Override a paused stage through the API.

~~~text
Usage: goobers-cloud override --api URL --idempotency-key KEY --decision NAME --rationale TEXT [--actor NAME] RUN STAGE

Override one paused stage through the control-plane API.
~~~

## `goobers-cloud runs list`

List runs through the API.

~~~text
Usage: goobers-cloud runs list --api URL [--gaggle NAME] [--workflow NAME] [--phase PHASE] [--limit N]

List cloud runs through the control-plane API.
~~~

## `goobers-cloud runs logs`

Read one run's journal events through the API.

~~~text
Usage: goobers-cloud runs logs --api URL RUN

Read one cloud run's journal events through the control-plane API.
~~~

## `goobers-cloud runs show`

Show one run through the API.

~~~text
Usage: goobers-cloud runs show --api URL RUN

Show one cloud run through the control-plane API.
~~~

## `goobers-cloud status`

Show cloud instance status through the API.

~~~text
Usage: goobers-cloud status --api URL

Show the cloud instance identity and status through the control-plane API.
~~~

## `goobers-cloud triggers`

Submit a durable workflow trigger through the API.

~~~text
Usage: goobers-cloud triggers --api URL --workflow NAME [--gaggle NAME] --idempotency-key KEY [--force] [--source-run RUN]

Submit one durable workflow trigger through the control-plane API.
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
