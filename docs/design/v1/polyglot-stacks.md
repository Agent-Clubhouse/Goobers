# Design: Polyglot stacks — .NET/C# first-class, Apple/Android laddered

> Status: **Draft for review** · Area: `RUN` / `WF` / `area:runner` · Milestone: **V1 —
> arbitrary repos, teams, hardening** (composes with **Custom & Generic Stages**, epic #744)
> References: `internal/executor/` (dispatch/shell/env), `internal/procenv/procenv.go`,
> `api/v1alpha1/gaggle_types.go`, `api/v1alpha1/workflow_types.go`, the per-gaggle CI-command
> issue #1009 (MGV-1), and the multi-gaggle design
> [`multi-gaggle-validation.md`](multi-gaggle-validation.md).
> Origin: the V1 breadth goal — a Goobers instance drives gaggles on non-Go repos (an Electron
> desktop app; an HTML/CSS/JS site; a .NET service) — with **C#/.NET first-class**, and a
> **designed ladder** for Apple (Swift/Obj-C + simulator) and Android with one validated target
> each.

## 1. Verdict

**The executor is already language-agnostic; "polyglot" is a config + provisioning problem,
not an executor rewrite.** `ShellExecutor` execs whatever argv the workflow YAML declares
(`internal/executor/shell.go:275`, `cmd.Dir = env.Workspace`, `cmd.Env = stageEnv`); nothing
in `internal/executor/` or `cmd/goobers/runnerwiring.go` hardcodes `make`/`go`/a CI command.
The Go bias lives in exactly three places, none of them the executor:

1. **The CI command is workflow-YAML config** — each gaggle's `implementation.yaml` literally
   declares `command: ["make", "ci"]` (`selfhost/gaggles/goobers/workflows/implementation.yaml:130`,
   `config-examples/gaggles/acme-web/workflows/implementation.yaml:98`). The real "hardcode" is a
   string in config, and `GaggleSpec` has no way to vary it (`api/v1alpha1/gaggle_types.go:9`).
2. **The env allowlist is Go-only** — `internal/procenv/procenv.go:27-36` is default-deny and
   passes `GO*` toolchain vars but no `DOTNET_ROOT`/`NUGET_PACKAGES`/`PYTHONPATH`/`NODE_PATH`,
   and is not gaggle-configurable. A `dotnet` stage using a non-default cache silently fails to
   resolve deps even with the binary on PATH.
3. **There is no toolchain provisioning** — a stage is a bare host subprocess
   (`shell.go:275`, `SysProcAttr{Setsid:true}`); the SDK must already be on the daemon host's
   `PATH` (the "host-PATH gambling" #735 names). The dead `DeterministicRun.Image` field
   (`api/v1alpha1/workflow_types.go:200`) is honored by nobody but the validator's VER003 warning
   (`internal/workflow/checks.go:48`).

So making **C#/.NET first-class** is: a per-gaggle CI command (#1009, approved), a toolchain-aware
env allowlist (#736), a declarative provisioning story (#735), a reference .NET gaggle that
proves it green — plus, optionally, containers (#734) and a kind registry (#737) as the durable
substrate. **No target-repo Go assumptions are hardcoded in Go source** (verified: the only
`go build`/`GOOS` sites are the daemon building *itself* and host-OS checks, e.g.
`internal/version/version.go`, `internal/sandbox/native_other.go:11`).

## 2. Scope

- **First-class this sprint: C#/.NET.** A .NET gaggle builds, tests, and ships green — on
  Linux/macOS and, per the Windows breadth goal, on **Windows CI** (pairs with milestone #17).
  "First-class" = declared CI command + toolchain env passthrough + a provisioning story +
  a shipped reference gaggle + a Windows CI leg that keeps it green.
- **Laddered, designed, one validated target each (not full support this sprint):**
  - **Apple — Swift/Obj-C:** one validated build target on a macOS host; **simulator
    automation designed** (#740, iOS Simulator/XCUITest). Xcode cannot be containerized, so
    Apple targets are **macOS-host-bound** and depend on the runner-requirement contract
    (#1087/#659) to schedule only where Xcode exists.
  - **Android:** one validated emulator target designed (#742), possibly containerizable with KVM.
- **Explicitly not this sprint:** full mobile CI matrices, MAUI/cross-compile toolchains,
  device farms, signing/notarization pipelines. Captured as ladder rungs, not built.

## 3. What exists vs. what this needs

| Seam | State | Issue | Needed |
|---|---|---|---|
| Per-gaggle CI command on `GaggleSpec` | ❌ Go string in YAML | **#1009 (approved/ready)** | land it — foundation |
| Toolchain env passthrough (`DOTNET_ROOT`, `NUGET_PACKAGES`, …) + gaggle-configurable | ❌ Go-only, `procenv.go:27` | #736 (unapproved) | **promote + implement (.NET first)** |
| Declarative runtime provisioning (`requires.runtime: {dotnet: "8.0"}`) | ❌ host-PATH gambling | #735 (unapproved sketch) | **promote + design the .NET path** |
| Containerized stage execution (revive dead `Image`) | ❌ dead field | #734 (unapproved) | escape hatch — sequence after env/provisioning |
| Registrable stage-kind seam | ❌ hardcoded 2-kind switch `dispatch.go:50` | #737 (unapproved) | enables `container`/remote kinds; refactor-only |
| **Reference .NET gaggle + green Windows CI** | ❌ nothing owns it | **NEW (capstone)** | **the "first-class" proof** |
| Apple/Swift validated target + simulator | ❌ | #740 | ladder rung, macOS-host-bound |
| Android validated target | ❌ | #742 (stretch) | ladder rung |
| Runner-requirement declaration (schedule-where-toolchain-exists) | ❌ no `requires`/`runsOn` field | #1087 / #659 | its own design pass — reference, don't duplicate |

## 4. Design

### P-1 — Per-gaggle CI command (#1009, foundation, approved)
Land MGV-1: a declared CI command per `GaggleSpec`, overridable per-workflow input, run by
`local-ci` instead of `["make","ci"]`. Non-zero exit fails the gate exactly as today; a bad
command only fails that gaggle's own PRs. Ship reference commands per stack as config examples
(the `.NET` example is P-4).

### P-2 — Toolchain-aware, gaggle-configurable env allowlist (#736)
Expand `procenv.Vars` with the .NET family first (`DOTNET_ROOT`, `DOTNET_CLI_TELEMETRY_OPTOUT`,
`DOTNET_NOLOGO`, `NUGET_PACKAGES`, `NUGET_HTTP_CACHE_PATH`), then Node/Python/Rust, and make the
allowlist **instance/gaggle-extendable** (still default-deny — an explicit list, never
`os.Environ()` passthrough). This is the cheapest single unblock for a .NET gaggle that has the
SDK installed but a non-default cache. **Additive, fail-closed preserved.**

### P-3 — Declarative runtime provisioning (#735), .NET path concretized
A stage/gaggle declares a `requires.runtime` (e.g. `{dotnet: "8.0"}`) resolved **before** the
command runs, fail-closed if unprovisionable. Two backing modes, deliberately a spectrum:
- **Host-resolved (near-term):** verify the declared SDK/version is present on the host PATH,
  fail-closed with a clear diagnostic if not (turns today's silent "command not found" into a
  declared, actionable requirement — and is the local sibling of the #1087 runner-requirement
  contract). No installation.
- **Container-provisioned (later):** select the toolchain via an image (#734) — the durable
  escape hatch #735 recommends, gated on #737's kind registry. Deferred behind the host mode.

### P-4 — Reference .NET/C# gaggle + green Windows CI (NEW capstone)
Nothing owns this today. Ship a real `config-examples/gaggles/<dotnet-service>/` with a .NET
`implementation.yaml` (CI command `dotnet build && dotnet test`, `requires.runtime: {dotnet}`),
a shipped-workflow contract test through the real runner + fake harness, and a **Windows CI leg**
that builds/tests it so .NET-on-Windows can't silently rot (mirrors #1091's discipline for the
daemon itself). AC includes the Go-only diagnostics caveat: `GOTRACEBACK`/SIGQUIT goroutine-dump
handling (`env.go:83`, `shell.go:342`) is inert for `dotnet test` — documented, not a blocker.

### P-5 — Apple + Android ladder (designed, one target each)
- **Apple:** #740 (iOS Simulator/XCUITest) + one validated Swift/Obj-C build target on a macOS
  host. Xcode is not containerizable → **macOS-host-bound**; scheduling correctness depends on
  the runner-requirement contract (§5). Design + one green e2e; full matrix deferred.
- **Android:** #742 emulator flavor, one validated target; KVM-containerizable exploration noted.

## 5. Runner requirements — reference, do not duplicate
Requirement "declare runner requirements, fail to schedule when unmet, same tests / different
per-platform outcomes" is **already owned**: #1087 (durable stage-level capability declaration
`requires`/`runsOn`, generalizing the routing key) and #659 (platform routing + fail-fast-vs-
queue semantics). Per the cross-platform planning, #1087 warrants **its own design pass** and is
not lumped into this doc. This design only *consumes* it: P-3's host-resolved mode is the
degenerate local case (verify-here-or-fail), and P-5's Apple targets are its first hard
constraint (Xcode-only-on-macOS). Near-term platform-varying test expectations ride the CI matrix
(#633) with build/platform tags, not the router.

## 6. Decomposition — dispatchable work items

| ID | Issue | Item | Risk | Status |
|---|---|---|---|---|
| PLY-1 | #1009 | Per-gaggle CI command (foundation) | Low | **approved/ready** |
| PLY-2 | #736 | Toolchain env allowlist (.NET first) + gaggle-configurable | Low-Med | promote → approve |
| PLY-3 | #735 | Declarative runtime provisioning; host-resolved .NET path | Med | promote → approve |
| PLY-4 | *(new)* | Reference .NET/C# gaggle + shipped-workflow test + Windows CI leg | Med | **new — the first-class proof** |
| PLY-5 | #734 | Containerized stage execution (revive `Image`) | Med-High | after PLY-2/3; escape hatch |
| PLY-6 | #737 | Registrable stage-kind seam (refactor-only) | Low | substrate for PLY-5 |
| PLY-7 | #740 | Apple: iOS simulator flavor + 1 Swift/Obj-C target (macOS-host-bound) | Med | laddered, designed |
| PLY-8 | #742 | Android: emulator flavor, 1 target | Med | laddered (stretch) |

## 7. Recommended sequencing
1. **Foundation:** PLY-1 (#1009) — already ready.
2. **First-class .NET core:** PLY-2 (#736 env) → PLY-3 (#735 host-resolved provisioning) →
   PLY-4 (reference gaggle + Windows CI leg). At the end of this leg a .NET gaggle builds and
   ships green on Linux/macOS/Windows — the sprint's first-class outcome.
3. **Durable substrate (opportunistic):** PLY-6 (#737 kind registry) → PLY-5 (#734 containers)
   — promotes provisioning from host-resolved to image-selected; not required for first-class .NET.
4. **Ladder (designed, one target each):** PLY-7 (#740 Apple) and PLY-8 (#742 Android), each
   gated on the runner-requirement contract (§5) for correct host scheduling.

## 8. Open questions (PO)
- **OQ-1 — provisioning bar for "first-class":** is host-resolved-fail-closed (P-3 near-term)
  sufficient to call .NET first-class this sprint, with container-provisioned (#734) deferred?
  *(Recommend: yes — host-resolved + a shipped Windows CI leg is a real, testable bar; containers
  are the durability follow-on.)*
- **OQ-2 — reference .NET gaggle target:** a minimal ASP.NET/service, or a console/library? *(Recommend:
  a small service with unit tests — exercises `dotnet build && dotnet test` and NuGet restore.)*
- **OQ-3 — Apple host availability:** confirm a macOS host with Xcode is available for the PLY-7
  validated target (nothing else can build/sign Apple code); if not, PLY-7 is design-only this sprint.
- **OQ-4 — promote #735/#736 now?** both are unapproved backlog sketches; first-class .NET
  requires them. *(Recommend: promote both to approved with the .NET-first scope above.)*
</content>
</invoke>
