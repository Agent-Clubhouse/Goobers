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
Usage: goobers-cloud worker [--task-queue <queue>]... [flags]

Host a Temporal worker for the tier-3 engine (experimental): connect to the
configured Temporal frontend, register the engine workflow and activities,
and serve the named task queue(s) until SIGTERM/SIGINT, then drain — stop
polling and let in-flight activities finish within --drain-timeout.
Child dispatch cleanup can take up to 5 additional minutes before exit.

The tier-3 engine is not on the local (V0) execution path; this command is
the deployable worker shape for the cloud ladder. Automated gate checks and
workspace provisioning (git worktrees + scratch dirs under --work-root) are
wired. With --instance, the worker also wires the same agentic and
deterministic executors as the local runner; without it, stages needing
those executors fail closed with a clear error.

Flags:
  --instance <dir>           instance root; wires the real agentic and
                             deterministic executors (default
                             $GOOBERS_INSTANCE_ROOT)
  --blob-store <dir>         directory backing the fleet-wide
                             content-addressed artifact store (default
                             $GOOBERS_BLOB_STORE)
  --blob-endpoint <url>      HTTP(S) blob plane; alternative to --blob-store
                             (default $GOOBERS_BLOB_ENDPOINT only when no
                             directory is selected). Instance-backed workers
                             require exactly one store mode.
  --task-queue <queue>       task queue to serve; repeatable (default
                             engine.taskQueue, with env override)
  --temporal-hostport <h:p>  Temporal frontend (default engine.hostPort,
                             with env override)
  --temporal-namespace <ns>  Temporal namespace (default engine.namespace,
                             with env override)
  --drain-timeout <dur>      graceful-drain bound after a shutdown signal
                             (default 30s)
  --health-addr <host:port>  serve process liveness at /healthz and worker
                             readiness at /readyz (disabled by default)
  --max-concurrent-activities <n>
                             maximum activities executed concurrently by
                             this worker process (default 1000)
  --work-root <dir>          root for stage workspaces (default: a
                             goobers-worker dir under the OS temp dir)
  --daemon-api <url>         daemon write API base URL; wires live journal
                             emission through the journal plane, with the
                             per-run bearer from $GOOBERS_POD_TOKEN when
                             set (default $GOOBERS_DAEMON_API)
  --config-reload-interval <dur>
                             how often to re-read the instance config
                             tree and rebuild the gaggle seams whose
                             config changed, without a restart; 0
                             disables reload and freezes the worker on
                             its boot-time tree (default 10s; requires
                             --instance)
  --config-endpoint <url>    daemon configuration plane; fetches sanitized
                             instance config by digest and updates it live
  --config-key-file <path>   shared signing key used only to mint worker
                             config-plane credentials
  --config-history-depth <n>
                             how many superseded config trees to retain
                             so an in-flight run pinned to one is still
                             served the goober kit it was admitted
                             against across a reload; 0 disables
                             retention and refuses every superseded pin
                             (default 3; requires --instance or
                             --config-endpoint)
  --dispatch-namespace <ns>  enables mode-3 stage dispatch: wires the
                             dispatcher behind the stage-dispatch seam and
                             serves the per-(gaggle x runner) dispatch
                             queues derived from the instance's runners:
                             inventory. Each stage pod is created in ITS
                             OWN gaggle's declared isolation.namespace
                             (#4897) — this flag's value no longer selects
                             the pod namespace and is kept only as the
                             non-empty signal that enables mode-3 dispatch.
                             Before polling or dispatching, the worker
                             verifies every declared gaggle namespace
                             exists and that this worker's credentials
                             hold the RBAC grants dispatch needs there,
                             failing startup by name otherwise. Requires
                             --instance and one artifact store mode. In
                             endpoint mode the worker reads surrendered
                             results through --daemon-api; cluster access
                             uses in-cluster credentials or the standard
                             kubeconfig rules (default
                             $GOOBERS_DISPATCH_NAMESPACE)

The worker identity reported to Temporal is versioned
(goobers-worker/<build>@<host>#<pid>) so visibility alone answers which
build serves a queue.

With --instance, the worker re-reads its local config tree; with
--config-endpoint, it fetches changed snapshots by digest. On either path,
the worker checks on
--config-reload-interval and atomically replaces the gaggle, credential,
and agentic-kit seams whose config changed, so a definitions edit reaches
the NEXT stage this worker serves without a pod restart. An attempt
already running keeps the kit it was handed. A reload that does not parse
is logged and rejected; the last-known-good tree stays in force.

An agentic stage is served the goober kit its run pinned at start
(run.yaml's gooberDigest), resolved against the current config tree or
one of the --config-history-depth superseded trees still retained. A pin
no retained tree satisfies is REFUSED by name (gate_pin_missing), loudly
and retriably, naming the expected digest: the worker never substitutes
its currently-configured goober for the one the run was admitted
against. Such an attempt recovers by itself once a reload brings the
pinned tree into force.

Exit codes: 0 = clean drain, 1 = startup/connection error, 2 = usage error,
3 = drain timeout expired with in-flight work abandoned.
~~~

**Examples**

~~~console
$ goobers-cloud worker --task-queue goobers
~~~
