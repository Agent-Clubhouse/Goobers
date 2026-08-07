# Quickstart (tier 1, local)

This is the canonical first-run path. Follow the sections in order: start with
a credential-free local demo, graduate to a disposable GitHub-backed run, then
create a regular instance and adopt production-oriented configuration. See
`docs/ARCHITECTURE.md` §6 for the instance layout these commands operate on.

If declarative systems are new to you, read
[How Goobers works: desired state, not scripts](../concepts/README.md) first.
It explains the config/runtime split and why agents propose definition changes
through pull requests.

## Build the binary

```sh
go build -o bin/goobers ./cmd/goobers    # or: make build
```

## 1. Run the zero-credential demo

The hermetic demo uses mock providers and requires no repository, provider
credentials, model tokens, or network writes. It is supported on Linux and
macOS, where Goobers enforces network isolation.

For host setup differences, see the [macOS](quickstart-macos.md),
[Linux](quickstart-linux.md), or [Windows](quickstart-windows.md) guide. Native
Windows cannot enforce the demo's network isolation; use its documented WSL 2
path instead.

```sh
bin/goobers init --demo ./demo-instance
bin/goobers run demo ./demo-instance
bin/goobers dashboard ./demo-instance
bin/goobers trace <run-id> ./demo-instance
```

The dashboard opens the Portal for the demo instance so you can follow the run
and inspect its workflow. The run flows through curate -> implement -> review,
passes the automated `review-verdict` gate, and produces a merge-preview
artifact before finishing — fully deterministic and offline, with no pause for
user input. `run` prints the run ID used by `trace`; the trace shows the
complete journal, including the gate's recorded verdict.

## 2. Graduate to the token-bearing quickstart template

Next, use the versioned `quickstart@v1` template for a first autonomous run
against a disposable GitHub repository. This path requires a GitHub token and
an authenticated Copilot CLI. Copy the paired sample into a separate throwaway
directory:

```sh
bin/goobers onboarding stub-sample \
  --destination ./getting-started-task-api \
  --json
```

The action is non-interactive, embeds the release-matched sample, and is safe to
re-run. It reports every created or skipped file plus the destination and next
command. It refuses conflicting user-owned files unless `--force` is explicit.
To also seed the catalog's labels and issues into an existing disposable GitHub
repository, add `--work-tracking owner/repo`; the command reads
`GOOBERS_GITHUB_ISSUES_TOKEN` by default. With no target or no configured token,
the JSON envelope reports the issues pending and still materializes the local
sample without network access. It never creates or pushes a remote.

```sh
bin/goobers init --template=quickstart ./tutorial-instance
bin/goobers validate ./tutorial-instance
bin/goobers run quickstart ./tutorial-instance
bin/goobers dashboard ./tutorial-instance
```

To seed the same template as a checked-in config source without runtime state,
use the non-interactive source-tree action. Its JSON result lists every created
or preserved file and the validation command to run next:

```sh
bin/goobers init --template=quickstart --source-tree ./tutorial-config --json
bin/goobers validate --source-tree --json ./tutorial-config
```

The dashboard opens the same Portal against this first GitHub-backed run. This
linear template claims one approved issue, implements it, performs an
advisory code-review task, pushes the run branch, and opens a pull request. It
is **not for production**: it intentionally omits CI gates, remediation loops,
bounded escalation, merge policy, and issue close-out so the onboarding happy
path has no stall points.

Continue with section 3 to configure a regular instance and run its selected
canonical workflow. Once that works, read the
[`config-examples` reference layout](../../config-examples/README.md) and adapt
its
[`implementation` workflow](../../config-examples/gaggles/acme-web/workflows/implementation.yaml)
for production-oriented review, local CI with bounded implementation repasses,
explicit escalation paths, and PR CI polling. Add the separate `merge-review`
workflow only after those safeguards are configured.

## 3. `init --guided` — configure a regular instance

```sh
export PATH="$PWD/bin:$PATH"
goobers init --guided ./my-instance
```

The guided flow uses the same configuration sequence as the release installer.
It separately selects a checked-in config source and target GitHub application
repository, prompts for credential references and canonical workflows, and
validates both the source and materialized instance. Use a fresh instance path;
a new config source path must also be empty, while an existing source is
validated and left unchanged. Guided init is first-run only and refuses an
already initialized target before prompting.

A fresh successful initialization records
`init.completed` in `scheduler/events.jsonl` as the Time to First PR anchor.

After validation, guided mode prints the config-source-to-instance mapping and
the commands for applying later source edits:

```text
After editing the checked-in source, validate and materialize it before startup:
  goobers validate --source-tree "<config-source>"
  goobers config materialize "<instance-root>"
  goobers up "<instance-root>"
```

It then prints the runnable next commands and developer documentation:

```text
Ready to run from <instance-root>:
  goobers up
  goobers run <workflow>

Developer docs:
  Author workflows:         https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/dsl-authoring-skill.md
  Make custom agent stages: https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/requirements/goober.md and https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/stage-contract.md
  View journal telemetry:   https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/cli/README.md (`goobers trace` / `goobers telemetry`)
```

Run those commands from the printed instance root. Tagged release binaries use
the corresponding release tag instead of `main` in the documentation URLs.

### Manual/advanced alternative: bare `init`

Use bare init when you intentionally want to scaffold and edit every
configuration layer yourself:

```sh
goobers init ./my-instance
```

This creates `instance.yaml`, a starter `config/` (one gaggle, one goober, one
`default-implement` workflow), and the empty `gaggles/`, `scheduler/`, and
`telemetry.db` placeholders (ARCHITECTURE.md §6). The daemon creates each
gaggle's `runs/` and `workcopies/` beneath `gaggles/<gaggle>/`. Bare init is safe
to re-run because existing pieces are left untouched.

Before starting the instance, edit `my-instance/instance.yaml` to point at your
repository and set the referenced provider token (env var or file, never inline;
CFG-009/SEC-010). Edit `my-instance/config/` to shape the workforce: the gaggle's
`project` and `backlog` repo references, the goober's
`harness`/`skills`/`tools`, and the workflow's `triggers`/`tasks`/`gates`. Then
validate the manual configuration:

```sh
goobers validate ./my-instance
```

`validate` checks `instance.yaml` and every document under `config/` against the
canonical schemas. Exit codes are `0` for valid configuration, `1` for
validation errors, and `2` for usage or I/O errors.

### Prerequisites for regular workflows

- `golangci-lint` must be on the **daemon's** `PATH` — a workflow's `local-ci`
  stage (`make ci` -> `lint`) runs as a subprocess of the daemon, not your
  interactive shell, so it inherits the daemon's `PATH`, not your dotfile's.
- The daemon passes through the Go toolchain env family (`GOPATH`, `GOBIN`,
  `GOCACHE`, `GOMODCACHE`, `GOFLAGS`, `GOPROXY`, `GOSUMDB`, `GOPRIVATE`,
  `GOTOOLCHAIN`) into every stage — set these before `goobers up` if your host
  relocates the Go cache/module store or sits behind a corporate module proxy.

**Upgrading a flat V0 instance:** on first startup, an instance with one active
gaggle automatically moves populated top-level `runs/` and `workcopies/` into
`gaggles/<gaggle>/` when no scoped runtime state exists yet. If several gaggles
are active, Goobers preserves the populated flat directories as a compatibility
root: retained journals remain readable and resumable while new runs use scoped
directories. Goobers also keeps that root separate if the configuration later
returns to one gaggle, because mixed historical state cannot be assigned safely.
Operators may relocate retained journals by their recorded gaggle during a
maintenance window; retained Git workcopies should stay at their legacy paths.

For event-driven workflows, see [GitHub webhook triggers](github-webhooks.md).
The daemon keeps that listener on loopback; tunnel or reverse-proxy exposure is
an operator choice.

## 4. `up` — run the daemon

```sh
cd ./my-instance
goobers up
```

Runs the daemon: the embedded scheduler (cron triggers + run conditions, #21)
driving the local runner (#17) — restarting any run left interrupted by a
prior crash or unclean shutdown via `Runner.Resume` before admitting new
work, and draining in-flight runs gracefully on SIGINT/SIGTERM rather than
killing them mid-attempt (#23). Blocks until interrupted; exit code `0` on a
clean shutdown, `1` if the daemon fails to start (e.g. another `up` already
holds this instance's lock). While running, it prints a liveness heartbeat
with scheduler activity once per minute; pass `--quiet` to suppress the
heartbeat while retaining startup and shutdown messages.

`instance.yaml` is read once, at startup — editing it while `up` is running
(a new repo, a `runConditions` change, etc.) has no effect until you restart
the daemon. How definitions reach the materialized `config/` directory depends
on the configured source:

- With the default instance-local config, `config/` is also read once at
  startup. Pass the opt-in `goobers up --watch-config` flag to watch direct
  edits to that directory. Valid edits swap in atomically; invalid edits leave
  the last-known-good definitions active.
- A Git `workflowSource` continuously reconciles its tracked ref without
  `--watch-config`. Periodic fetch-and-compare polling is always active, local
  Git ref changes wake reconciliation immediately, and authenticated GitHub
  push deliveries also wake it when `webhook.secret` is configured. Invalid
  revisions are rejected while the last-known-good definitions keep running.

In-flight runs stay pinned to the definitions they started with in either mode.

To trigger one workflow manually instead of starting the daemon, use the other
command from the guided banner:

```sh
goobers run <workflow>
```

This honors run conditions (max-parallel, budgets), pins the workflow's compiled
digest, creates its run journal (ARCHITECTURE.md §4), and advances it through the
local runner. It prints the run ID up front and the final phase and state when it
returns.

## 5. `status` — list runs

```sh
goobers status
```

`status` revalidates the active configuration before listing runs. On a new
instance its first-run success indicator waits for the first PR; after the
workflow journals its first PR open, it celebrates
`First-run success: first PR in <duration>`. The duration is read from the
successful-init `init.completed` event and the first PR-open `ref.touched`
event, not from command timing. Non-fatal
configuration warnings use the same `WARNING <code> <scope>: <explanation>`
lines printed during `up` startup. `status --json` returns an object with
`warnings` and `runs` arrays plus the structured `timeToFirstPR` metric; warning
objects contain `code`, `severity`, `scope`, and `explanation`.

```
RUN ID                              WORKFLOW                  GAGGLE      PHASE       STARTED
a671b69fe766595e550677b91658726a    default-implement         example     completed   2026-07-12T23:37:36-07:00
```

## 6. `trace` — inspect one run

```sh
goobers trace a671b69fe766595e550677b91658726a
```

Prints the run's pinned identity, current phase/checkpoint, and every journal
event in order (`run.started`, `stage.*`, `gate.evaluated`, `ref.touched`,
`error`, `run.finished`, …) — the same fields the `cat`/`jq` debugging recipes
in `internal/journal/README.md` use, just pre-formatted. If the telemetry
rollup (`telemetry.db`, #22) has ingested the run, its trace spans print too;
this is best-effort — an empty or not-yet-rebuilt rollup is not an error.

## 7. `reset-rate-limit` — run again without losing history

A workflow's `maxRunsPerHour` budget can leave you rate-limited when you want to
trigger another run immediately (e.g. during acceptance testing). Reset just the
hourly budget — **never** `rm -rf ./my-instance` to clear it:

```sh
goobers reset-rate-limit
```

This writes a small marker under `scheduler/` that moves the rate window's floor
to now, so the next `goobers up`/`goobers run` starts with a fresh budget. It
**preserves `gaggles/*/runs/`** — the append-only run journals that are the durable
execution record (`trace` reads them). Wiping the instance root to reset the
rate window destroys those journals as a side effect; this command doesn't.
Stop the daemon first if one is running — the reset is applied when the
scheduler next reconstructs its budget window at startup.

## Exit codes

Every subcommand follows the same convention: `0` = OK, `1` = validation/
business error (invalid config, unknown workflow), `2` = usage/IO error (bad
flags, not an instance root, missing run).
See also: [V0-ACCEPTANCE.md](../V0-ACCEPTANCE.md) — the end-to-end acceptance runbook that assembles these commands into a full live run.

For the production-oriented path from a foreign GitHub repository through
curation and an implementation PR, including multi-gaggle configuration, see
[Onboard an arbitrary repository](arbitrary-repo-onboarding.md).
