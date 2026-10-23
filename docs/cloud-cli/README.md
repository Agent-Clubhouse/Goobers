# `goobers-cloud` CLI reference

<!-- Generated from the command registry by `make docs`. Do not edit by hand; edits are overwritten and the CI drift guard will fail. -->

`goobers-cloud` is the cloud control-plane role host. This reference is generated from the CLI command registry, so it always matches the shipped binary.

## Core commands

| Command | Description |
| --- | --- |
| [`goobers-cloud api`](#goobers-cloud-api) | Serve the cloud control-plane API. |
| [`goobers-cloud approve`](#goobers-cloud-approve) | Approve a paused stage through the API. |
| [`goobers-cloud bootstrap`](#goobers-cloud-bootstrap) | Prepare a new cloud install for cutover. |
| [`goobers-cloud cancel`](#goobers-cloud-cancel) | Cancel a live run through the API. |
| [`goobers-cloud claims list`](#goobers-cloud-claims-list) | List active claims through the API. |
| [`goobers-cloud claims release`](#goobers-cloud-claims-release) | Release a cloud claim. |
| [`goobers-cloud completion`](#goobers-cloud-completion) | Generate a shell completion script. |
| [`goobers-cloud config apply`](#goobers-cloud-config-apply) | Apply cloud configuration. |
| [`goobers-cloud config status`](#goobers-cloud-config-status) | Show cloud configuration status. |
| [`goobers-cloud control`](#goobers-cloud-control) | Serve API and the single-active cloud controller. |
| [`goobers-cloud controller`](#goobers-cloud-controller) | Run cloud control-plane reconciliation. |
| [`goobers-cloud cutover`](#goobers-cloud-cutover) | Check cutover readiness of a cloud install. |
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
  --instance-id ID                 Required instance (or GOOBERS_INSTANCE_ID)
  --database-url URL               PgBouncer URL (prefer env/file for credentials)
  --database-url-file PATH         Read PgBouncer URL from a mounted Secret
  --database-direct-url URL        Direct URL (prefer env/file for credentials)
  --database-direct-url-file PATH  Read direct URL from a mounted Secret
  --database-max-conns N           Total connection budget (default 16)
  --database-direct-max-conns N    Direct-pool reservation (default 2)
  --database-auth MODE             password or entra (default password)
  --object-store-url URL           Azure Blob service URL
  --object-store-container NAME    Blob container (default goobers)
  --blob-gc-pitr-retention DURATION  Minimum restore retention (default 840h)
  --experimental-physical-blob-gc  Enable storage-leased physical deletion (#7843)

Environment:
  GOOBERS_INSTANCE_ID          Required non-placeholder instance identity
  GOOBERS_DATABASE_URL         PgBouncer transaction-pool URL
  GOOBERS_DATABASE_DIRECT_URL  Direct PostgreSQL URL
  GOOBERS_OBJECT_STORE_URL     Azure Blob service URL
~~~

## `goobers-cloud approve`

Approve a paused stage through the API.

~~~text
Usage: goobers-cloud approve --api URL --idempotency-key KEY [--actor NAME] [--rationale TEXT] RUN STAGE

Approve one paused or escalated stage through the control-plane API.
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
  --admin-url URL                  Server administrator URL: creates roles and databases
  --admin-url-file PATH            Read the administrator URL from a mounted Secret
  --entra-principal ROLE=OBJECTID  Create ROLE as an Entra principal (repeatable)
  --database-direct-url URL        Instance owner URL: runs migrations (prefer env/file)
  --database-direct-url-file PATH  Read the owner URL from a mounted Secret
  --database-auth MODE             password or entra, for the owner (default password)

The administrator and owner are separate identities: the administrator never
runs migrations and the owner never creates roles.

Environment:
  GOOBERS_DATABASE_ADMIN_URL   Administrator PostgreSQL URL
  GOOBERS_DATABASE_DIRECT_URL  Owner PostgreSQL URL
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
backend supplies an operator-safe release operation (#7808; implementation PR
#7845).
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

Apply a cloud configuration generation. Reserved until #7834.
~~~

## `goobers-cloud config status`

Show cloud configuration status.

~~~text
Usage: goobers-cloud config status [OPTIONS]

Show cloud configuration generation and eligibility status. Reserved until #7834.
~~~

## `goobers-cloud control`

Serve API and the single-active cloud controller.

~~~text
Usage: goobers-cloud control [DATABASE OPTIONS] [OPTIONS]

Serve API and the single-active, fenced cloud controller. Never executes stages.
Database options (separate credentials; no role membership):
  --instance-id ID                          Required instance (or GOOBERS_INSTANCE_ID)
  --api-database-url URL                    API transaction-pool URL
  --api-database-url-file FILE              API URL file
  --api-database-direct-url URL             API direct URL
  --api-database-direct-url-file FILE       API direct URL file
  --controller-database-url URL             Controller transaction-pool URL
  --controller-database-url-file FILE       Controller URL file
  --controller-database-direct-url URL      Controller direct URL
  --controller-database-direct-url-file FILE Controller direct URL file
  --database-auth MODE                      password or entra (default password)
  --database-max-conns N                    Combined budget (default 16)
  --database-direct-max-conns N             Direct reservation per role (default 2)

Environment: GOOBERS_API_DATABASE_URL, GOOBERS_API_DATABASE_DIRECT_URL,
GOOBERS_CONTROLLER_DATABASE_URL, GOOBERS_CONTROLLER_DATABASE_DIRECT_URL.
Both roles must target goobers_<instance>, with dashes changed to underscores.

Options:
  --check-assembly              Print compiled plane wiring and exit (offline)
  --listen ADDRESS              API/probes address (default 127.0.0.1:8080)
  --tls-cert FILE                API TLS certificate (required off loopback)
  --tls-key FILE                 API TLS private key
  --pod-key-file FILE            Shared signed pod-token key file (required)
  --self-execution POLICY        Must be deny (default deny)
  --temporal-hostport HOST:PORT  Temporal address (default localhost:7233)
  --temporal-namespace NAME      Temporal namespace (default default)
  --task-queue NAME              Remote engine task queue (default goobers)
  --object-store-url URL         Azure Blob service URL (or GOOBERS_OBJECT_STORE_URL); required: the
                                 journal span source and config generations live there
  --object-store-container NAME  Azure Blob container (default goobers)

Readiness requires schema compatibility, complete plane/recovery wiring, and a
live controller lease. SIGTERM stops controller loops and releases authority.
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

## `goobers-cloud deny`

Deny an escalated run through the API.

~~~text
Usage: goobers-cloud deny --api URL --idempotency-key KEY --rationale TEXT [--actor NAME] RUN

Deny one escalated run through the control-plane API.
~~~

## `goobers-cloud lease status`

Show cloud controller lease status.

~~~text
Usage: goobers-cloud lease status [DATABASE OPTIONS]

Show active cloud controller leases, fence epochs, DB-clock expiry, and recent
lease history.
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
Usage: goobers-cloud migrate status [DATABASE OPTIONS]

Show applied cloud schema migrations and binary compatibility.
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
