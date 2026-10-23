# `goobers-cloud` CLI reference

<!-- Generated from the command registry by `make docs`. Do not edit by hand; edits are overwritten and the CI drift guard will fail. -->

`goobers-cloud` is the cloud control-plane role host. This reference is generated from the CLI command registry, so it always matches the shipped binary.

## Core commands

| Command | Description |
| --- | --- |
| [`goobers-cloud api`](#goobers-cloud-api) | Serve the cloud control-plane API. |
| [`goobers-cloud completion`](#goobers-cloud-completion) | Generate a shell completion script. |
| [`goobers-cloud controller`](#goobers-cloud-controller) | Run cloud control-plane reconciliation. |
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
  --database-url URL               PgBouncer transaction-pool URL
  --database-direct-url URL        Direct PostgreSQL URL for session-bound work
  --database-max-conns N           Total connection budget (default 16)
  --database-direct-max-conns N    Direct-pool reservation (default 2)
  --database-auth MODE             password or entra (default password)
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

## `goobers-cloud migrate`

Apply cloud data-store migrations.

~~~text
Usage: goobers-cloud migrate

Apply cloud data-store migrations. The P1 skeleton reserves this role while
the migration runner is wired into the cloud composition root.
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
