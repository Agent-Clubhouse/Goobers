# Quickstart tutorial (tier 1, local)

This is the canonical **learning path**, not the procedure for configuring a
real application repository. Follow sections 1 and 2 in order: start with a
credential-free local demo, then graduate to a disposable GitHub-backed run.
Delete those tutorial repositories and instances when you are done.

If your goal is to configure Goobers for an existing repository, skip this
tutorial and run `goobers init --guided`, then follow
[Onboard an arbitrary repository](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/arbitrary-repo-onboarding.md). That path
adapts the production-oriented canonical workflow modules to your repository.

| Goal | Route |
| --- | --- |
| See the workflow model without credentials | Section 1: zero-credential demo |
| Exercise one disposable issue-to-PR run | Sections 1-2: complete tutorial |
| Configure a real repository | `goobers init --guided` and [Onboard an arbitrary repository](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/arbitrary-repo-onboarding.md) |
| Hand-author every configuration layer | [Manual configuration](#manualadvanced-alternative-bare-init) |

**Before continuing, complete installation and host prerequisites in the guide
for your operating system:** [macOS](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/quickstart-macos.md),
[Linux](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/quickstart-linux.md), or [Windows](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/quickstart-windows.md). Return here
for the disposable tutorial after the OS guide says the `goobers` binary and
required host tools are ready.

See `docs/ARCHITECTURE.md` §6 for the instance layout these commands operate on.

If declarative systems are new to you, read
[How Goobers works: desired state, not scripts](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/concepts/README.md) first.
It explains the config/runtime split and why agents propose definition changes
through pull requests.

## Build the binary

```sh
go build -o bin/goobers ./cmd/goobers    # or: make build
```

## Pick an agent harness

Every agentic goober needs one configured harness: GitHub Copilot CLI or
Claude Code CLI. Install and sign in to whichever one your goobers declare
(`harness: copilot` or `harness: claude-code` in their `goober.yaml`) before
section 2 below — the deterministic demo in section 1 needs neither. For
Claude Code:

```sh
npm install -g @anthropic-ai/claude-code
claude auth login
```

For host setup differences (Homebrew paths, WSL 2, launchd/systemd PATH
quirks), see the platform guide linked in section 1 below. Section 2
shows how to confirm Goobers can see whichever harness you installed.

## 1. Run the zero-credential demo

The hermetic demo uses mock providers and requires no repository, provider
credentials, model tokens, or network writes. It is supported on Linux and
macOS, where Goobers enforces network isolation.

For host setup differences, see the [macOS](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/quickstart-macos.md),
[Linux](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/quickstart-linux.md), or [Windows](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/quickstart-windows.md) guide. Native
Windows cannot enforce the demo's network isolation; use its documented WSL 2
path instead.

```sh
bin/goobers init --demo ./demo-instance
bin/goobers run demo ./demo-instance
```

`run` waits for the run to reach a terminal state by default, so by the time
it returns the demo has already flowed through curate -> implement -> review,
passed the automated `review-verdict` gate, and produced a merge-preview
artifact — fully deterministic and offline, with no pause for user input.
`run` prints the run ID used by `trace`.

`dashboard` blocks until interrupted, so open it in a second terminal to
browse the run in the Portal:

```sh
# second terminal
bin/goobers dashboard ./demo-instance
```

Press Ctrl-C in that terminal when you're done. Back in the first terminal,
inspect the complete journal, including the gate's recorded verdict:

```sh
bin/goobers trace <run-id> ./demo-instance
```

## 2. Graduate to the token-bearing quickstart template

Next, use the versioned `quickstart@v1` template for a first autonomous run
against a disposable GitHub repository you control. This path requires a
GitHub token and an authenticated agent harness. The shipped template's
goobers default to `harness: copilot`; to run it on Claude Code instead, set
`harness: claude-code` in `./tutorial-instance/config/gaggles/example/goobers/{implementer,reviewer}/goober.yaml`
after materializing the instance below (see
[`config-examples/gaggles/acme-web-claude`](https://github.com/Agent-Clubhouse/Goobers/blob/main/config-examples/gaggles/acme-web-claude/)
for a full claude-code gaggle reference).

### Check prerequisites

The sample's CI command requires Node.js 20 or newer and npm. Confirm both are
available on the same `PATH` Goobers will use before materializing the sample:

```sh
node --version
npm --version
```

The first command must report `v20.0.0` or newer, and both commands must
succeed. At run start, Goobers preflights the configured `npm` CI executable
before any workflow stage executes. If npm is missing, the run fails before it
claims or changes an issue with a `ciCommand executable "npm" not found` error;
install Node.js 20+ and npm, then run the command again. The preflight checks
that npm exists, not the Node.js major version, so do not skip the literal
version checks above.

### Materialize the sample and the instance

Copy the paired sample into a separate throwaway directory, then scaffold the
instance that will operate on it:

```sh
bin/goobers onboarding stub-sample \
  --destination ./getting-started-task-api \
  --json
bin/goobers init --template=quickstart ./tutorial-instance
```

`stub-sample` is non-interactive, embeds the release-matched sample, and is
safe to re-run; it refuses conflicting user-owned files unless `--force` is
explicit, and never creates or pushes a remote itself. Its `--json` output is
a versioned action envelope:

```json
{
  "action": "stub-sample",
  "version": 2,
  "created": [".github/workflows/ci.yml", "package.json", "..."],
  "skipped": [],
  "path": "/absolute/path/to/getting-started-task-api",
  "nextCommand": "goobers init --template=quickstart ./tutorial-instance"
}
```

`created` lists paths written in this run; `skipped` lists paths already
present. `nextCommand` is the next command to run. `init --template=quickstart`
materializes `./tutorial-instance` still pointing at the template's
placeholder repository (`your-org/your-repo`); the next step replaces that
with a real one.

### Create a disposable repository and connect the instance to it

1. Create a new, empty GitHub repository to hold the sample, and push it —
   any name, delete it whenever you're done. With the GitHub CLI:

   ```sh
   gh repo create <owner>/<repo> --private --source ./getting-started-task-api --push
   ```

   Without it, create the repository at <https://github.com/new>, then:

   ```sh
   git -C ./getting-started-task-api init -b main
   git -C ./getting-started-task-api add -A
   git -C ./getting-started-task-api commit -m "Getting Started sample"
   git -C ./getting-started-task-api remote add origin https://github.com/<owner>/<repo>.git
   git -C ./getting-started-task-api push -u origin main
   ```

   Already have a disposable repository you'd rather reuse? Skip this step
   and use its `<owner>/<repo>` below instead.

2. Export a GitHub token with repo/issues access, once, under the name
   `connect` expects by default:

   ```sh
   export GOOBERS_GITHUB_TOKEN=<your token>
   ```

3. Point the instance at the repository, and seed it in the same step:

   ```sh
   bin/goobers connect <owner>/<repo> --seed ./tutorial-instance
   ```

   `connect` rewrites the placeholder `your-org/your-repo` in
   `./tutorial-instance`'s `instance.yaml` and gaggle config to the repository
   you gave it, records `GOOBERS_GITHUB_TOKEN` (or the name you passed via
   `--token-env NAME`, if you keep the token under a different variable) as
   the credential reference by name only — the value never passes through
   this command — and validates the result in-process. `--seed` derives the
   labels the quickstart workflow's backlog selector requires, ensures they
   exist on the repository, and files one safe starter issue, using that same
   token — one `GOOBERS_GITHUB_TOKEN` export covers connecting and seeding.
   Configuration already pointing at a real repository is left alone unless
   you pass `--replace`.

4. Confirm Goobers can see and use your installed harness before the first
   run — `--check-harness` preflights every harness referenced by the
   instance's goobers and prints `HARNESS claude-code: OK` (or `HARNESS
   copilot: OK`) once the CLI is installed and signed in:

   ```sh
   bin/goobers validate --check-harness ./tutorial-instance
   ```

### Run it

```sh
bin/goobers run quickstart ./tutorial-instance
```

`run` waits for the run to reach a terminal state by default. This is a real
autonomous run against your disposable repository, so it takes noticeably
longer than the offline demo: it claims one approved issue, implements it,
performs an advisory code-review task, pushes the run branch, and opens a
pull request. It is **not for production**: it intentionally omits CI gates,
remediation loops, bounded escalation, merge policy, and issue close-out so
the onboarding happy path has no stall points.

`dashboard` blocks until interrupted, so open it in a second terminal to
browse the run in the Portal, and press Ctrl-C there when you're done:

```sh
# second terminal
bin/goobers dashboard ./tutorial-instance
```

To seed the same template as a checked-in config source without runtime state,
use the non-interactive source-tree action. Its JSON result lists every created
or preserved file and the validation command to run next:

```sh
bin/goobers init --template=quickstart --source-tree ./tutorial-config --json
bin/goobers validate --source-tree --json ./tutorial-config
```

The browser wizard is intentionally not an alternative tutorial. Use
`goobers init --guided` when you are ready to configure a real
repository with the production-oriented canonical workflow modules.

The tutorial is complete after this disposable run. Do not promote the
`quickstart@v1` workflow into production: it intentionally omits safeguards.
To configure a real repository, use `goobers init --guided` and
[Onboard an arbitrary repository](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/arbitrary-repo-onboarding.md).

For reference, the production-oriented path starts from the
[`config-examples` reference layout](../../config-examples/README.md) and adapt
its
[`implementation` workflow](../../config-examples/gaggles/acme-web/workflows/implementation.yaml)
for production-oriented review, local CI with bounded implementation repasses,
explicit escalation paths, and PR CI polling. Add the separate `merge-review`
workflow only after those safeguards are configured.

## Separate path: configure a real instance

This section is not the next tutorial step. It summarizes the separate,
production-oriented path documented fully in
[Onboard an arbitrary repository](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/arbitrary-repo-onboarding.md).

Start the focused browser walkthrough:

```sh
export PATH="$PWD/bin:$PATH"
goobers init --guided
```

When the GitHub Copilot app is already installed, the guided flow also offers
the release-matched Goobers Portal canvas extension. Declining does not affect
setup. Install it later (or after installing the app) with `goobers
portal-extension install`; after upgrading Goobers, use `goobers
portal-extension status` and `goobers portal-extension update`.

Provide an existing local Git clone. Getting Started supports GitHub and Azure
DevOps, discovers repository identity, default branch, CI command, toolchain,
and existing CLI authentication, then asks only for configuration placement,
workflow behavior, and agent harness choices that cannot be derived.

The workflow choices are adapted from the canonical modules under
[`config-examples/gaggles/acme-web`](../../config-examples/gaggles/acme-web/),
not from the deliberately simplified `quickstart@v1` tutorial workflow.

The default configuration location is a peer folder beside the application
repository. You can instead choose a `goobers` folder inside the repository or
another local path. Mutable instance state remains outside the application
repository. Track the configuration folder with Git for review and history.

To use an agent instead of the browser, install the release-matched agent
toolkit and run this prompt from the selected configuration folder:

```text
Use the Goobers Getting Started skill to inspect target repository <path-or-provider-url>,
derive its default branch, CI command, toolchain, and conventions, and create the
smallest validated configuration source here. Explain each write and ask only when
required evidence or behavior cannot be safely derived.
```

A fresh successful initialization records
`init.completed` in `scheduler/events.jsonl` as the Time to First PR anchor.

After validation, setup shows the config-source-to-instance mapping and the
commands for applying later source edits:

```text
After editing the checked-in source, validate and materialize it before startup:
  goobers validate --source-tree "<config-source>"
  goobers config materialize "<instance-root>"
  goobers up "<instance-root>"
```

It also shows runnable next commands, repository-aware customization prompts,
and developer documentation:

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
- `GOMAXPROCS` is *derived*, not just passed through. When the daemon runs in a
  container whose CPU quota is narrower than the machine's CPU count, every
  stage and harness subprocess is told that quota, so `go build -p`/`go test -p`
  size their process fan-out to what the container can actually run instead of
  to the node's CPU count. Nothing is set on an unconstrained host, and a
  `GOMAXPROCS` you set yourself — on the daemon, or in a stage's own `env:` —
  always wins.

**Upgrading a flat V0 instance:** on first startup, an instance with one active
gaggle automatically moves populated top-level `runs/` and `workcopies/` into
`gaggles/<gaggle>/` when no scoped runtime state exists yet. If several gaggles
are active, Goobers preserves the populated flat directories as a compatibility
root: retained journals remain readable and resumable while new runs use scoped
directories. Goobers also keeps that root separate if the configuration later
returns to one gaggle, because mixed historical state cannot be assigned safely.
Operators may relocate retained journals by their recorded gaggle during a
maintenance window; retained Git workcopies should stay at their legacy paths.

For event-driven workflows, see [GitHub webhook triggers](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/github-webhooks.md).
The daemon keeps that listener on loopback; tunnel or reverse-proxy exposure is
an operator choice.

## Operating a configured instance

The remaining commands apply after Getting Started or manual configuration;
they are not additional quickstart tutorial steps.

### `up` — run the daemon

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

### `status` — list runs

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

### `trace` — inspect one run

```sh
goobers trace a671b69fe766595e550677b91658726a
```

Prints the run's pinned identity, current phase/checkpoint, and every journal
event in order (`run.started`, `stage.*`, `gate.evaluated`, `ref.touched`,
`error`, `run.finished`, …) — the same fields the `cat`/`jq` debugging recipes
in `internal/journal/README.md` use, just pre-formatted. If the telemetry
rollup (`telemetry.db`, #22) has ingested the run, its trace spans print too;
this is best-effort — an empty or not-yet-rebuilt rollup is not an error.

### `reset-rate-limit` — run again without losing history

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
See also: [V0-ACCEPTANCE.md](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/V0-ACCEPTANCE.md) — the end-to-end acceptance runbook that assembles these commands into a full live run.

For the production-oriented path from a foreign GitHub repository through
curation and an implementation PR, including multi-gaggle configuration, see
[Onboard an arbitrary repository](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/arbitrary-repo-onboarding.md).
