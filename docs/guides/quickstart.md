# Quickstart (tier 1, local)

Walks the full `goobers` CLI surface end to end: scaffold an instance, point it
at your repo, and trigger a run. See `docs/ARCHITECTURE.md` §6 for the instance
layout these commands operate on.

## Prerequisites

- `golangci-lint` must be on the **daemon's** `PATH` — a workflow's `local-ci`
  stage (`make ci` -> `lint`) runs as a subprocess of the daemon, not your
  interactive shell, so it inherits the daemon's `PATH`, not your dotfile's.
- The daemon passes through the Go toolchain env family (`GOPATH`, `GOBIN`,
  `GOCACHE`, `GOMODCACHE`, `GOFLAGS`, `GOPROXY`, `GOSUMDB`, `GOPRIVATE`,
  `GOTOOLCHAIN`) into every stage — set these before `goobers up` if your host
  relocates the Go cache/module store or sits behind a corporate module proxy.

## 1. Build the binary

```sh
go build -o bin/goobers ./cmd/goobers    # or: make build
```

## 2. `init` — scaffold an instance root

```sh
bin/goobers init ./my-instance
```

Creates `instance.yaml`, a starter `config/` (one gaggle, one goober, one
implement-only workflow), and the empty `runs/`, `scheduler/`, `workcopies/`,
`telemetry.db` placeholders (ARCHITECTURE.md §6). Safe to re-run — existing
pieces are left untouched.

## 3. Configure

Edit `my-instance/instance.yaml` to point at your own repo and set the
referenced provider token (env var or file — never inline, CFG-009/SEC-010).
Edit `my-instance/config/` to shape your workforce: the gaggle's `project`
and `backlog` repo references, the goober's `harness`/`skills`/`tools`, and the
workflow's `triggers`/`tasks`/`gates`.

## 4. `validate` — check it

```sh
bin/goobers validate ./my-instance
```

Checks `instance.yaml` and every document under `config/` against the
canonical schemas. Exit codes: `0` valid, `1` validation errors, `2` usage/IO
error (e.g. not an instance root yet).

## 5. `up` — run the daemon

```sh
bin/goobers up ./my-instance
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
the daemon. Definitions under `config/` can be watched and reloaded live with
the opt-in `goobers up --watch-config` flag (off by default): after a valid edit
the new definitions swap in atomically, and an invalid edit leaves the
last-known-good definitions active. Without the flag, `config/` is also read once
at startup. (Live watch is experimental and will be superseded by the Workflow CD
config source, #453.)

## 6. `run` — trigger one manually

```sh
bin/goobers run default-implement ./my-instance
```

Triggers a run of the named `config/` workflow manually, still honoring run
conditions (max-parallel, budgets). Pins the workflow's compiled digest,
creates its run journal (ARCHITECTURE.md §4), and advances it through the
real local runner — deterministic tasks execute in a fresh worktree, agentic
tasks/gates invoke the goober's harness (Copilot CLI by default) — blocking
until the run reaches a terminal state or pauses (e.g. a human gate). Prints
the run id up front and the final phase/state once it returns.

## 7. `status` — list runs

```sh
bin/goobers status ./my-instance
```

`status` revalidates the active configuration before listing runs. Non-fatal
configuration warnings use the same `WARNING <code> <scope>: <explanation>`
lines printed during `up` startup. `status --json` returns an object with
`warnings` and `runs` arrays; warning objects contain `code`, `severity`,
`scope`, and `explanation`.

```
RUN ID                              WORKFLOW                  GAGGLE      PHASE       STARTED
a671b69fe766595e550677b91658726a    default-implement         example     completed   2026-07-12T23:37:36-07:00
```

## 8. `trace` — inspect one run

```sh
bin/goobers trace a671b69fe766595e550677b91658726a ./my-instance
```

Prints the run's pinned identity, current phase/checkpoint, and every journal
event in order (`run.started`, `stage.*`, `gate.evaluated`, `ref.touched`,
`error`, `run.finished`, …) — the same fields the `cat`/`jq` debugging recipes
in `internal/journal/README.md` use, just pre-formatted. If the telemetry
rollup (`telemetry.db`, #22) has ingested the run, its trace spans print too;
this is best-effort — an empty or not-yet-rebuilt rollup is not an error.

## 9. `reset-rate-limit` — run again without losing history

A workflow's `maxRunsPerHour` budget can leave you rate-limited when you want to
trigger another run immediately (e.g. during acceptance testing). Reset just the
hourly budget — **never** `rm -rf ./my-instance` to clear it:

```sh
bin/goobers reset-rate-limit ./my-instance
```

This writes a small marker under `scheduler/` that moves the rate window's floor
to now, so the next `goobers up`/`goobers run` starts with a fresh budget. It
**preserves `runs/`** — the append-only run journals that are the durable
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
