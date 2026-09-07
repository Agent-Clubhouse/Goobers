# Stack support

Goobers' runner and workflow DSL are stack-neutral by design: a gaggle declares what its
target repository needs, and the same mechanism runs it regardless of language. This guide
states that boundary precisely — what's built into Goobers vs. what a gaggle must declare —
and lists which stacks have a shipped, proven reference gaggle today.

See
[`docs/design/v1/polyglot-stacks.md`](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/design/v1/polyglot-stacks.md)
for the underlying design rationale; this guide documents the resulting
operator-facing mechanism, not the design history.

## Stack-neutral (built into Goobers, no per-stack code)

- **The executor.** `ShellExecutor` (`internal/executor/shell.go`) execs whatever argv a
  deterministic stage declares, directly via `os/exec` — no language, file-extension, or
  toolchain branching. It special-cases only `command[0] == "goobers"` (substituting the
  daemon's own binary path and injecting run-identity env vars), which is CLI self-identity
  handling, not stack detection.
- **The workflow DSL and scheduler.** Tasks, gates, triggers, and readiness controls carry no
  language-specific concepts.
- **The capability-claim / requirement-match model (RRQ-1, #1101).** A runner advertises the
  toolchains it claims (`instance.yaml`'s `runner.capabilities`); a gaggle or task declares
  what it needs (`requiredCapabilities`). The scheduler refuses to place a run whose
  requirement isn't claimed, failing at *schedule* time with a diagnostic naming the missing
  capability — never a mid-run "command not found."
- **The toolchain preflight (#735).** Before a run's first stage executes, the runner
  host-probes each claimed toolchain the run actually needs (`internal/toolchain`) against the
  real host, and fails the run closed with a diagnostic if a runner *falsely* claimed a
  capability it doesn't actually have. Probed families today: `dotnet`, `node`, `python`, `go`,
  `java`, and `os=<goos>`. A family with no registered prober (e.g. `xcode`, `netfx@4.8`) is
  matched only at schedule time — never host-probed — and is skipped by the preflight.
- **The env-allowlist *framework*** (`internal/procenv`). A default-deny allowlist of exact
  env-var names carried from the daemon into stage subprocesses. The framework is stack-neutral
  — which toolchain families' variables it lists, and any per-instance additions, are what's
  actually stack-specific (see the escape hatch below).

## Stack-specific (what a gaggle must declare)

- **`ciCommand`** (`GaggleSpec.CICommand`, MGV-1/#1009) — the argv the `local-ci` stage runs
  for this gaggle, applied at config-load time before workflows compile. A gaggle that
  declares no `ciCommand` falls back to whatever its own workflow's `local-ci` task declares
  directly — which, in a workflow copied from the Go reference, is `["make", "ci"]`. Every
  shipped non-Go reference gaggle declares its own (`["npm", "run", "ci"]` for Node,
  `["dotnet", "test"]` for .NET, `["mvn", "-B", "-q", "verify"]` for Java, and
  `["python3", "-m", "pytest", "-q"]` for Python).

  Each of those gaggles' own workflows now declares the **same** stack-native argv as the
  gaggle's `ciCommand` (#2554). The resolution still happens — that is what lets one workflow
  template serve any stack — but a reader of the example is no longer shown a Go command that
  is silently replaced at load time. `test/stackparity` enforces both halves: the `local-ci`
  literal must match the gaggle's declared `ciCommand`, and a non-Go example may not carry a
  Go-toolchain literal in any stage command without an explicit
  `# stackparity:go-fallback <reason>` marker naming why.
- **`requiredCapabilities`** — the toolchain tokens the gaggle or task needs (`node@20`,
  `dotnet@9`, `python@3.12`, `java@21`, `os=windows`, …), matched at schedule time and,
  where a prober exists for that family, re-verified against the actual host before the run's
  stages execute.
- **A starting-point reference gaggle**, for stacks that have one — copy it rather than
  building a gaggle definition from scratch (see the tier table below).

If a toolchain family needs an environment variable `internal/procenv` doesn't allowlist by
default, declare it explicitly via instance config (`RunnerConfig.EnvPassthrough`, consumed by
`procenv.BaseEnvWith`) — never by switching a stage to unrestricted `os.Environ()`
passthrough. This is the escape hatch for a toolchain family with no built-in allowlist
entries yet, and it still fails closed: a malformed entry is rejected at config-load, not at
stage-launch.

For first-class Node stages, Goobers preserves `NPM_CONFIG_REGISTRY` and
`NPM_CONFIG_REPLACE_REGISTRY_HOST`. Set these to route npm installs through an
operator-configured registry when a `package-lock.json` contains absolute
`registry.npmjs.org` URLs. Credential-bearing `npm_config_*` variables are not
preserved as a family; configure npm authentication through the runner's
credential mechanisms instead.

## Current tiers

| Stack | Tier | Reference | Status |
|---|---|---|---|
| Go | First-class — shipped reference + executed CI leg | Goobers canonical reference (`reference-workflows/gaggles/goobers/`) | Shipped, CI-green |
| Java | First-class — shipped reference + executed CI leg | `config-examples/gaggles/java-service/` | Shipped, CI-green |
| .NET/C# | First-class — shipped reference, leg is opt-in | `config-examples/gaggles/dotnet-service/` | Shipped, validated locally |
| Python | First-class — shipped reference, leg is opt-in | `config-examples/gaggles/python-service/` | Shipped, validated locally |
| Node/TypeScript | First-class — shipped reference, no executing leg | `config-examples/gaggles/acme-web/` | Shipped reference, structural checks only |
| Apple/iOS | Laddered — one validated target | simulator automation stage flavor | Landed (#740) |
| Android | Laddered — one validated target, stretch | emulator automation stage flavor | Open, stretch (#742) |
| Anything else | Bring-your-own | — | Declare `ciCommand` + `requiredCapabilities`; runs through the same mechanism, no shipped reference yet |

### What each status actually claims

The three "Shipped" statuses are different claims, and the difference is the
one the table used to blur (#2555). All three mean a real reference gaggle
ships in this repository and is schema-validated and compiled by `make ci`.
They differ in what *executes* it:

- **Shipped, CI-green** — an end-to-end test drives the real runner over the
  shipped gaggle, running its actual `ciCommand` against a real project, and
  **CI runs that test on every pull request**. Go's leg is this repository's
  own suite; Java's is `test/e2e/java_gaggle_integration_test.go`, which the
  `declared-dependency integration` job enables by setting `GOOBERS_JAVA_E2E`
  and provisions with `setup-java` plus a warmed Maven repository.
- **Shipped, validated locally** — the same end-to-end test exists
  (`test/e2e/dotnet_gaggle_integration_test.go`,
  `test/e2e/python_gaggle_integration_test.go`) and passes on a host with the
  toolchain, but it is opt-in (`GOOBERS_DOTNET_E2E`, `GOOBERS_PYTHON_E2E`) and
  **nothing in CI sets those variables**. Pinning the .NET SDK and a Python
  3.12 + pytest environment in the cloud runner is the remaining work
  ([#4615](https://github.com/Agent-Clubhouse/Goobers/issues/4615)); until that
  lands, the row must not claim CI evidence it does not have.
- **Shipped reference, structural checks only** — the gaggle and its workflows
  are validated and compiled in CI, but no test executes the stack's own
  toolchain. Node/TypeScript is here: `acme-web` is the flagship
  PR-lifecycle example, and nothing runs `npm run ci` against a real Node
  project in CI.

Promoting a row is one change, not two: `test/stackparity` fails if the
published status and the checked-in evidence disagree in either direction. Turn
on a stack's opt-in variable in `.github/workflows/ci.yml` without relabelling
the row and the gate says so; relabel the row without turning it on and it says
that too.

A "first-class" entry means a shipped reference gaggle exercising the real workflow
machinery, at one of the three evidence levels above. "Laddered" and "bring-your-own"
stacks work today the same way any stack does — declare the two config fields above — they
just don't have a shipped reference gaggle.
