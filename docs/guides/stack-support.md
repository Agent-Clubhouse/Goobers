# Stack support

Goobers' runner and workflow DSL are stack-neutral by design: a gaggle declares what its
target repository needs, and the same mechanism runs it regardless of language. This guide
states that boundary precisely — what's built into Goobers vs. what a gaggle must declare —
and lists which stacks have a shipped, proven reference gaggle today.

See [`docs/design/v1/polyglot-stacks.md`](../design/v1/polyglot-stacks.md) for the underlying
design rationale; this guide documents the resulting operator-facing mechanism, not the design
history.

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
  directly — typically the Go-stack default, `["make", "ci"]`, left over in a copied reference
  workflow. Every shipped non-Go reference gaggle overrides it (`["npm", "run", "ci"]` for
  Node, `["dotnet", "test"]` for .NET, and `["mvn", "-B", "-q", "verify"]`
  for Java).
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
| Go | First-class — shipped reference + green test | Goobers canonical reference (`reference-workflows/gaggles/goobers/`) | Shipped, green |
| .NET/C# | First-class — shipped reference + green test | `config-examples/gaggles/dotnet-service/` | Shipped, green |
| Node/TypeScript | First-class — shipped reference + green test | `config-examples/gaggles/acme-web/` | Shipped, green |
| Java | First-class — shipped reference + green test | `config-examples/gaggles/java-service/` | Shipped, green |
| Python | Planned | `config-examples/gaggles/python-service/` | Reference gaggle open (#2170) |
| Apple/iOS | Laddered — one validated target | simulator automation stage flavor | Landed (#740) |
| Android | Laddered — one validated target, stretch | emulator automation stage flavor | Open, stretch (#742) |
| Anything else | Bring-your-own | — | Declare `ciCommand` + `requiredCapabilities`; runs through the same mechanism, no shipped reference yet |

A "first-class" entry means: a shipped `config-examples/gaggles/` reference gaggle exercising
the real workflow machinery, proven green. "Planned" stacks work today the same way
"bring-your-own" stacks do — declare the two config fields above — they just don't have a
shipped reference yet.
