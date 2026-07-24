# Quickstart (tier 1, local)

Walks the full `goobers` CLI surface end to end: scaffold an instance, point it
at your repo, and trigger a run. See `docs/ARCHITECTURE.md` §6 for the instance
layout these commands operate on.

If declarative systems are new to you, read
[How Goobers works: desired state, not scripts](../concepts/README.md) first.
It explains the config/runtime split and why agents propose definition changes
through pull requests.

## Prerequisites

- The onboarding `quickstart` template does not run local CI. If you graduate to
  the production `implementation` workflow, `golangci-lint` must be on the
  **daemon's** `PATH` because its `local-ci` stage runs as a daemon subprocess.
- The daemon passes through the Go toolchain env family (`GOPATH`, `GOBIN`,
  `GOCACHE`, `GOMODCACHE`, `GOFLAGS`, `GOPROXY`, `GOSUMDB`, `GOPRIVATE`,
  `GOTOOLCHAIN`) into every stage — set these before `goobers up` if your host
  relocates the Go cache/module store or sits behind a corporate module proxy.

## 1. Build the binary

```sh
go build -o bin/goobers ./cmd/goobers    # or: make build
```

## 2. `init` — scaffold an instance root

For a first autonomous tutorial cycle, use guided setup and accept its
`quickstart` workflow default:

```sh
bin/goobers init --guided ./my-instance
```

The guided flow defaults to the named `quickstart` template, version 1, authored
against DSL `1.4`. It configures a manual workflow that claims one issue carrying
the maintainer-applied `goobers:approved` trust label and the `goobers:ready`
label, implements it, performs one non-blocking advisory review of the published
commit, opens a pull request, and clears the claim marker.
The daemon creates each gaggle's `runs/` and `workcopies/` beneath
`gaggles/<gaggle>/`.

> **Onboarding only — not for production.** Quickstart intentionally omits a
> reviewer verdict policy, remediation branches, local and remote CI gates,
> escalation/parking, PR close-out, and merging. Its review task continues even
> when the reviewer returns a failure result; harness or malformed-envelope
> errors still fail closed. Use it only with a disposable tutorial target and
> inspect the resulting pull request before merging.

Plain `bin/goobers init ./my-instance` remains available for the smaller
implement-only starter. Both modes create `instance.yaml`, `config/`, and the
empty `gaggles/`, `scheduler/`, and `telemetry.db` placeholders
(ARCHITECTURE.md §6). Plain init is safe to re-run because existing pieces are
left untouched; guided setup is first-run only.

### Onboarding-only template

For a first autonomous run against a disposable tutorial target, seed the
versioned `quickstart@v1` template:

```sh
bin/goobers init --template=quickstart ./tutorial-instance
bin/goobers validate ./tutorial-instance
bin/goobers run quickstart ./tutorial-instance
```

This linear template claims one approved issue, implements it, performs an
advisory code-review task, pushes the run branch, and opens a pull request. It
is **not for production**: it intentionally omits CI gates, remediation loops,
bounded escalation, merge policy, and issue close-out so the onboarding happy
path has no stall points.

Graduate by scaffolding the canonical `implementation` workflow through
`goobers init --guided`, or by adapting
`config-examples/gaggles/acme-web/workflows/implementation.yaml`. That workflow
adds reviewer verdict policy, local CI with bounded implementation repasses,
explicit escalation paths, and PR CI polling. Add the separate `merge-review`
workflow only after those safeguards are configured.

**Upgrading a flat V0 instance:** on first startup, an instance with one active
gaggle automatically moves populated top-level `runs/` and `workcopies/` into
`gaggles/<gaggle>/` when no scoped runtime state exists yet. If several gaggles
are active, Goobers preserves the populated flat directories as a compatibility
root: retained journals remain readable and resumable while new runs use scoped
directories. Goobers also keeps that root separate if the configuration later
returns to one gaggle, because mixed historical state cannot be assigned safely.
Operators may relocate retained journals by their recorded gaggle during a
maintenance window; retained Git workcopies should stay at their legacy paths.

## 3. Configure

Guided setup writes the repository, branch, and token-reference names you
entered. Export those token environment variables in the shell that starts
Goobers, then review `my-instance/config/` before running. Token values belong
in environment variables or files, never inline (CFG-009/SEC-010). Plain init
users must also replace the placeholder repository references.

For event-driven workflows, see [GitHub webhook triggers](github-webhooks.md).
The daemon keeps that listener on loopback; tunnel or reverse-proxy exposure is
an operator choice.

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
bin/goobers run quickstart ./my-instance
```

Triggers a run of the named `config/` workflow manually, still honoring run
conditions (max-parallel, budgets). Pins the workflow's compiled digest,
creates its run journal (ARCHITECTURE.md §4), and advances it through the
real local runner — deterministic tasks execute in a fresh worktree, agentic
tasks/gates invoke the goober's harness (Copilot CLI by default) — blocking
until the run reaches a terminal state or pauses (e.g. a human gate). Prints
the run id up front and the final phase/state once it returns.

### Graduate to production safeguards

After the tutorial cycle, replace quickstart with the canonical
[`implementation.yaml`](../../config-examples/gaggles/acme-web/workflows/implementation.yaml)
using the [arbitrary-repository onboarding guide](arbitrary-repo-onboarding.md).
Graduate in this order: add the reviewer verdict policy, add remediation and CI
repass paths, add bounded escalation/parking, then opt into the separate
[`merge-review.yaml`](../../config-examples/gaggles/acme-web/workflows/merge-review.yaml)
only after the repository's merge policy is configured. The production
implementation workflow keeps pull-request opening separate and does not merge
the first PR itself.

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
