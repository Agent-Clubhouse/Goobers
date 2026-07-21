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

### P-3 — Declared runtime requirement, matched at *schedule* against a runner capability claim
**PO-confirmed model (unifies .NET, Apple, Windows — 2026-07-20):** assume the toolchain is
preinstalled; a **runner advertises the capabilities it claims** (`dotnet@8`, `xcode`,
`os=windows`); a gaggle/stage **declares its required capabilities** (#735); the **scheduler
refuses to place the workload on a runner that does not claim them — failing at *schedule* time
with a clear diagnostic**, not scheduling-then-failing-at-run. A runner that *lies* (claims
`dotnet` but lacks it) yields a **runtime error** — an accepted degradation, not something the
scheduler prevents. No installation is performed. This is the **near-term, statically-configured
slice of the #1087 runner-requirement contract**: **RRQ-1 (new issue)** owns the
runner-capability-claim + schedule-time match; #735 owns the requirement declaration.
Container-provisioned toolchains (revive `Image` #734 + kind-registry #737) are the durable
follow-on but are **out of scope this sprint** — captured as a forward exploration issue on
prepopulated container stages (§8).

### P-4 — Reference .NET/C# gaggle, validated on a provisioned host (NEW capstone)
Nothing owns this today. Ship a real `config-examples/gaggles/<dotnet-service>/` with a .NET
`implementation.yaml` (CI command `dotnet build && dotnet test`, `requires.runtime: {dotnet}`),
proven green through the **real runner + fake harness on a host that has the SDK** (the sprint
runs on a machine with .NET installed). Schedule-time fail-closed comes from RRQ-1/P-3: no
`dotnet`-claiming runner ⇒ the workload does not schedule. **Cloud CI pinning is soft/stretch
(PO):** an evergreen Windows and/or macOS CI leg keeping the reference gaggle green is desirable
for regression protection but **not required this sprint** — the daemon's own Windows support
keeps its own gate (#1091/#633); this is about pinning the *gaggle*. AC note: Go-only diagnostics
(`GOTRACEBACK`/SIGQUIT, `env.go:83`/`shell.go:342`) are inert for `dotnet test` — documented, not
a blocker.

### P-5 — Apple + Android ladder (same capability model, one target each)
**Apple is handled exactly like .NET (PO):** assume Xcode present, declare the `xcode` capability
requirement, fail-closed at schedule if no runner claims it (RRQ-1/§5), runtime error if a claim
is false. Xcode is not containerizable, so the only valid runner is a macOS host with Xcode — but
that is now just a capability claim, not special-casing.
- **Apple:** #740 (iOS Simulator/XCUITest + one Swift/Obj-C build). **This sprint validates the
  target on the local macOS+Xcode machine → a real green e2e; no cloud macOS runner required**
  (cloud macOS CI for pinned support is the same soft/stretch goal as .NET). Full matrix deferred.
- **Android:** #742 emulator flavor, one validated target, same model; KVM-containerizable noted.

## 5. Runner requirements — the near-term slice this sprint builds (RRQ-1)
The PO-confirmed provisioning model (§4 P-3) **is** a runner-requirement contract in its
near-term form, so this sprint builds the minimal slice: **RRQ-1 (new)** — a runner advertises a
static capability set, a gaggle/stage declares required capabilities, the scheduler
**fails-to-schedule** on an unmet requirement with a clear diagnostic, and a **false claim
degrades to a runtime error**. The **full** dynamic routing / capability-matched pools remain
**#1087's own design pass** (not lumped in here); #659 owns platform-pool routing. RRQ-1 is their
degenerate, statically-configured ancestor and the **shared substrate for PLY-3/PLY-4 (.NET) and
PLY-7 (Apple)** — one mechanism serves `dotnet`, `xcode`, and `os=windows`. Platform-varying
*test expectations* still ride the CI matrix (#633) with build/platform tags, not the router.

## 6. Decomposition — dispatchable work items

| ID | Issue | Item | Risk | Status |
|---|---|---|---|---|
| RRQ-1 | *(new)* | Runner capability claim + schedule-time requirement match (near-term slice of #1087) | Med | **new — shared substrate for .NET + Apple** |
| PLY-1 | #1009 | Per-gaggle CI command (foundation) | Low | ready |
| PLY-2 | #736 | Toolchain env allowlist (.NET first) + gaggle-configurable | Low-Med | in sprint |
| PLY-3 | #735 | Declared runtime requirement (consumed by RRQ-1's schedule match) | Med | in sprint |
| PLY-4 | *(new)* | Reference .NET/C# gaggle validated on a provisioned host; cloud CI pinning soft | Med | **new — the first-class proof** |
| PLY-5 | #734 | Containerized stage execution (revive `Image`) | Med-High | **deferred** (see §8 forward issue) |
| PLY-6 | #737 | Registrable stage-kind seam (refactor-only) | Low | substrate for PLY-5 (deferred) |
| PLY-7 | #740 | Apple: iOS simulator + 1 Swift/Obj-C target, validated on local Xcode host | Med | laddered, real e2e this sprint |
| PLY-8 | #742 | Android: emulator flavor, 1 target | Med | laddered (stretch) |

> `sprint:v1-breadth` is the work queue; `goobers:approved` is a *daemon*-pickup flag and does
> not gate this human/agent-worker sprint. Items above run off the label regardless of approval.

## 7. Recommended sequencing
1. **Foundation:** PLY-1 (#1009, ready) + **RRQ-1** (runner capability claim + schedule match) —
   RRQ-1 is the substrate the rest depends on for fail-at-schedule.
2. **First-class .NET core:** PLY-2 (#736 env) → PLY-3 (#735 requirement declaration) →
   PLY-4 (reference gaggle, validated on a .NET-provisioned host). At the end of this leg a .NET
   gaggle builds and ships green with schedule-time fail-closed — the sprint's first-class outcome.
3. **Ladder (one target each, same capability model):** PLY-7 (#740 Apple, validated on the local
   Xcode host) and PLY-8 (#742 Android) — both consume RRQ-1 for correct host scheduling.
4. **Deferred (forward issue, §8):** PLY-6 (#737 kind registry) → PLY-5 (#734 containers) —
   prepopulated container stages are a later investment, not this sprint.

## 8. Resolved decisions (PO, 2026-07-20)
- **Provisioning bar — RESOLVED:** assume the toolchain is **preinstalled**; declare the runtime
  requirement; **fail at schedule** against a runner's capability claim (RRQ-1), not
  schedule-then-fail-at-run; a false claim degrades to a runtime error. Container-provisioned
  toolchains are **deferred** (forward issue below). This is the bar for "first-class" this sprint.
- **Cloud CI pinning — RESOLVED soft/stretch:** an evergreen Windows/macOS CI leg pinning the
  reference .NET / Apple gaggles is desirable but **not required this sprint**.
- **Apple = same model as .NET — RESOLVED:** assume Xcode present; capability-claim + fail-closed
  at schedule. **Validated on the local macOS+Xcode machine this sprint** (real green e2e); **no
  cloud macOS runner required.**
- **Mixed-mode as a canonical pattern — RESOLVED: not now** (Goobers is already *incidentally*
  mixed-mode; that is awareness, not a green light). #804/#369 stay deferred; addable later.
- **Approval labels are moot** for this sprint — it runs off `sprint:v1-breadth`, not
  `goobers:approved` (a daemon-pickup flag). No approval changes needed.

### Forward exploration (later, not this sprint)
- **Prepopulated container stages** — container stages seeded with the right branch/code/runtime
  (composes #734 image execution + #737 kind registry + #735 declarations). Filed as a Future
  exploration issue; the durable answer to hermetic, version-pinned toolchains beyond preinstall.
- **OQ (still open) — reference .NET target:** minimal ASP.NET service vs console/library?
  *(Recommend a small service with unit tests — exercises `dotnet build && dotnet test` + NuGet restore.)*
</content>
</invoke>
