# Mixed-Platform Cloud Nodes — Windows Node Pools & Platform-Labeled Routing

**Status:** implemented for Temporal stage routing; node-pool provisioning remains
operator-managed (issue #659, P13 of `docs/design/cross-platform-support.md` §3).

**Locked decisions (Lead ruling, 2026-07-25, recorded on #659):** the platform
label is a **stage-level** attribute, an unlabeled stage defaults to **linux**, and the
scheduler **fails fast** with a clear diagnostic when no node matches — no
queue-and-wait. The Temporal engine implements those decisions with per-activity task
queues and a finite schedule-to-start timeout (§2); the local scheduler remains
run-granular.

---

## 1. Context

`docs/design/cross-platform-support.md` §2 sets Goobers' platform strategy: Phase L makes
Linux officially supported and is explicitly the tier-3 prerequisite ("cloud nodes
(tier-3 workers/pods) are Linux-first"); Phase W ports the daemon to Windows through a
build-tagged platform-abstraction layer. §3's P13 entry is the note this document
elaborates:

> Mixed-platform cloud nodes (design) — tier-3 note: Linux pods are the default execution
> substrate; Windows *worker nodes* matter only for teams whose build/test requires
> Windows — shape: Windows node pool + task-queue routing by platform label. Design doc +
> conformance implications; no implementation until a customer shape demands it.

This document retains that deployment posture: **Linux pods remain the default and only
substrate most gaggles ever see.** Windows worker nodes are an opt-in pool that exists
only for stages whose toolchain genuinely requires Windows (Windows-only build systems,
.NET Framework, driver/desktop builds) — this is not a general multi-platform scheduler,
it is a narrow escape hatch for a substrate Goobers already treats as second-class outside
that one case. The customer shape subsequently materialized, and the Temporal routing
described here shipped and was exercised in a mixed-OS cluster.

## 2. Routing model

### 2.1 Where the label lives

Goobers already has a stage-level, free-form runner-requirement mechanism — RRQ-1/#1101,
`internal/runnercap`, described in `docs/design/v1/polyglot-stacks.md` §5. `Task` declares
`RequiredCapabilities []string` (`api/v1alpha1/workflow_types.go:165-174`), whose own doc
comment already names `os=windows` as an example token. A runner statically advertises a
claimed set via `runner.capabilities` in `instance.yaml`
(`internal/instance/config.go` `RunnerConfig.Capabilities`), and `internal/runnercap.Claimed`
matches requirement against claim.

**Decision: the platform label is not a new field. It is the well-known capability token
`os=<goos>` (`os=windows`, `os=linux`) inside the existing `Task.RequiredCapabilities`
and `GaggleSpec.RequiredCapabilities` lists**, not a bespoke `Task.Platform` field. Two
mechanisms for one concept would let them drift (a stage could declare `os=windows` in
one field and something contradictory in the other); one vocabulary, already schedule-time
enforced, is cheaper. The routing implementation therefore reused the existing token
rather than adding a new contract surface.

- **Declaration**: `Task.RequiredCapabilities` (`api/v1alpha1/workflow_types.go:174`) is
  a per-stage list, so no schema change is needed to declare which stage needs a
  platform. Declaration granularity is not execution granularity, however:
  `WorkflowRequiredCapabilities` unions the gaggle's requirements and every stage's
  requirements when the local scheduler admits a run
  (`internal/instance/gagglecapability.go:59-137`). The one local runner that claims that
  union then executes every stage in the workflow.
- **Who stamps it**: whoever authors the workflow definition (the same author who writes
  any other `RequiredCapabilities` entry, e.g. `dotnet@8`) — no new authoring surface, no
  separate approval path.
- **Default**: in the Temporal stage-level router, a stage with no `os=*` token in its
  resolved `RequiredCapabilities` is **unlabeled ⇒ linux**. The current local admission
  check does not inject an implicit `os=linux` into the workflow-level union, so a Linux
  runner's claimed set need not enumerate it. Only `os=windows` (or a future non-Linux
  `os=*` value) is meaningfully "missing" today.
- **Reservation, not a new schema field**: nothing here requires a work-item contract
  change. If future routing needs find `os=*` too coarse (e.g.
  needing an architecture qualifier), that is a follow-up issue filed against
  `RequiredCapabilities`'s existing free-form vocabulary, not a reason to add a field now.

### 2.2 Mixed-platform pipelines: routed per stage in Temporal

The Temporal engine routes each stage from its own `Task.RequiredCapabilities`. An
unlabeled stage (or one labeled `os=linux`) inherits the workflow task queue; a stage
labeled `os=windows` is dispatched to `<workflow-queue>-windows`. The workflow itself
stays on its original queue, so one run can span Linux and Windows workers without a
multi-queue workflow starter.

Every stage dispatch also has a finite schedule-to-start timeout. If no worker polls the
selected platform queue, the activity fails within that bound and names the queue instead
of waiting indefinitely. The local scheduler remains run-granular as described in §2.3.

### 2.3 Interim (local) scheduler mapping

`internal/localscheduler`'s existing schedule-time admission check (§5 below) treats
`os=windows` as an ordinary capability token, but only at run granularity. The interim
scheduler has exactly one runner identity per daemon process (today's single-runner
model). A workflow with any Windows-labeled stage must therefore run in full on a daemon
whose `instance.yaml` claims `os=windows`; the local scheduler cannot split its stages
between daemons. Cross-daemon run distribution is out of scope for the interim scheduler
generally (single-runner model, `docs/ARCHITECTURE.md` §3.1) and stays out of scope here.

### 2.4 V2 (Temporal) task-queue mapping

`docs/design/v2-cloud-scale.md` Workstream G2 partitions Tier 3 by mapping gaggles to
Temporal task queues + worker deployments per gaggle, so one hot gaggle cannot starve
others. Platform routing is an implemented **orthogonal second axis on the same
queue-naming scheme**:

- Queue name stays `<workflow-queue>` for the default (Linux) case — byte-identical to
  G2's existing scheme, so a gaggle that never uses `os=windows` sees no change at all —
  and becomes `<workflow-queue>-windows` for a stage whose capabilities include
  `os=windows`.
- A Windows worker deployment (§3) polls only `<workflow-queue>-windows`-suffixed queues
  for the gaggles it serves; it never claims the unsuffixed queue.
- Dispatch-time queue selection reads the current stage's `Task.RequiredCapabilities`;
  it does not use the run-level `WorkflowRequiredCapabilities` union.
- `engine.NewTemporalStarter` continues to accept one workflow task queue. Individual
  activities select their platform queue through `ActivityOptions.TaskQueue`, so the
  workflow itself does not need to move between queues.
- Every stage dispatch sets a finite schedule-to-start bound. Expiration names the
  unmatched task queue instead of permitting the activity to wait indefinitely.

## 3. Node-pool shape

A Windows worker is a **distinct, opt-in pool** — never a default fleet member:

- **Provisioning expectation**: persistent Windows VMs, not Windows containers, for v1 of
  this design. Windows containers (process-isolated) interact with the still-open P11
  (#651) isolation-posture decision; committing to a containerized Windows worker shape
  ahead of that decision would bake in an isolation posture nothing has ruled on yet. This
  section is explicitly **provisional** pending #651.
- **What a Windows worker must provide**, mapped to this milestone's own prerequisite
  workstreams:
  - Git floor + worktree settings per P9 (#643) — **closed**, so this input is settled:
    the audited git/worktree behavior (long paths, `core.symlinks`, CRLF) is the baseline
    a Windows node's git installation must meet.
  - Daemon supervision per P8 (#639) — **closed**: a Windows node runs the same
    supervised-install shape P8 delivered (Windows service, not systemd/launchd).
  - Agent harness per P10 (#647) — **open, blocked** ("NATIVE_WINDOWS_UNAVAILABLE": P10
    needs a native Windows host with an authenticated Copilot CLI to even attempt its
    spike). A Windows node pool cannot be conformant until P10 resolves one way or the
    other; this section is **provisional** pending #647.
  - Isolation/sandbox posture per P11 (#651) — **open, undecided**. Per
    `cross-platform-support.md` §4's own acceptance shape ("sandboxing posture stated per
    platform \[...] even where the statement is 'none yet'"), a Windows node's
    conformance checklist entry for sandboxing is **provisional: "none yet — trusted local
    only, logged"** until #651 lands, exactly mirroring how §4 already permits that
    interim statement for other platforms.

## 4. Conformance contract

`cross-platform-support.md` §4 states the milestone's acceptance shape for a platform in
general:

> - CI matrix green (required) on ubuntu/macos/windows for `go run ./test/ci`.
> - Documented, supervised daemon install on all three platforms.
> - Implementation-workflow e2e (fake harness) proven per platform; live-smoke documented
>   where the agent harness supports the platform (P10 outcome).
> - Sandboxing posture stated per platform (even where the statement is "none yet —
>   trusted local only, logged").

Turned into a **per-node conformance checklist** a scheduler could trust before routing
`os=windows`-labeled work to a candidate node:

| # | Check | Source | Status today |
|---|---|---|---|
| 1 | `go run ./test/ci` green on this node's platform build | §4 bullet 1 | Windows CI matrix job exists per Phase W; node itself must match it |
| 2 | Daemon installed via the documented, supervised path (Windows service) | §4 bullet 2, P8 #639 | Closed — settled |
| 3 | Git/worktree settings meet the P9 audit baseline | P9 #643 | Closed — settled |
| 4 | Agent harness installed, authenticated, and passes the P10 reality-check verdict | §4 bullet 3, P10 #647 | **Open — provisional**, node cannot be conformant until resolved |
| 5 | Sandbox posture explicitly stated (even if "none yet — trusted local only, logged") | §4 bullet 4, P11 #651 | **Open — provisional**, defaults to the explicit "none yet" statement §4 already allows |
| 6 | Runner advertises `os=windows` in `runner.capabilities` (`instance.yaml`) | §2.1 | Mechanism exists today (RRQ-1/#1101); no new work |

A node that fails check 1–3 or 6 is not eligible to join the pool at all. A node that
"passes" only by falling back to check 4/5's provisional statements is eligible but
**must surface that provisional status in its advertised capability set or accompanying
node metadata** (e.g. `os=windows` alone, with no additional harness/sandbox-verified
token) — good practice for future observability (§5), not a new mechanism this doc
specifies further.

## 5. Failure & scheduling semantics

**Decision (Lead ruling): fail fast, not queue-and-wait.** The local runner provides this
behavior at whole-run admission:

`internal/localscheduler/scheduler.go` (around line 1121) already refuses to schedule a
run whose resolved `RequiredCapabilities` are not a subset of the runner's claimed set:

```go
if missing := s.runnerCapabilities.Missing(entry.RequiredCapabilities); len(missing) > 0 {
    reason := ReasonMissingCapability + ": " + strings.Join(missing, ", ")
    s.journalEvent(journal.Event{Type: journal.EventTickSkipped, ...})
    span.Complete(telemetry.OutcomeBlocked, false)
    return "", false, reason
}
```

The code comment at that site calls this "the load-bearing seam a future
dynamic/multi-runner router grows from." A workflow with an `os=windows` stage and no
Windows claim on the local runner is refused with `missing capability: os=windows`
before any stage starts. If the runner does claim it, the whole workflow runs there.

The Temporal engine provides the equivalent contract at stage dispatch. It selects the
activity queue from the stage's platform capability and applies a finite
schedule-to-start bound, so an unmatched stage fails within a defined interval with a
diagnostic naming the queue rather than remaining queued.

**Observability**: the refusal is already a first-class journal event
(`journal.EventTickSkipped`) with a `Reason` string naming the missing token — this is
sufficient for the execution record to answer "why didn't this run start" without new
telemetry. Temporal timeout diagnostics name the selected per-platform queue. Recording
every successful queue-selection decision alongside the run remains a possible
observability enhancement, not a prerequisite for routing.

## 6. Implementation trigger (met)

Per `cross-platform-support.md` §3's original gate ("no implementation until a customer
shape demands it") and #659's scope, this section defined a concrete trigger rather than
leaving "customer demand" as an unfalsifiable placeholder:

**The trigger is: a specific gaggle, with a specific workflow, has at least one stage
whose build/test toolchain cannot run on Linux** (a genuine Windows-only dependency —
.NET Framework, a Windows-only build tool, driver/desktop code — not merely "the team
prefers Windows"), **and that gaggle is already active** (not hypothetical/prospective).
That case subsequently existed and was exercised in a real mixed-OS cluster. The shipped
shape is:

1. The gaggle's workflow author adds `os=windows` to the relevant stage's
   `RequiredCapabilities` (§2.1). No schema change is needed for this authoring half.
2. A Windows worker polls the `<workflow-queue>-windows` queue used by stages declaring
   `os=windows`; unlabeled and `os=linux` stages stay on the workflow queue.
3. Temporal dispatch resolves each stage's platform capability and applies the
   schedule-to-start failure bound (§2.2). The local runner still either refuses the whole
   run or executes every stage on a Windows-capable runner.

No dates, capacity planning, or provisioning automation are proposed here — this section
records the condition that authorized the now-shipped routing work. P9/P10/P11 and the
provisional node-pool concerns in §3 remain separate from routing.

## 7. Contract reservations

None. §2.1 deliberately reuses the existing `RequiredCapabilities` free-form vocabulary
rather than reserving a new field, and the implementation did not add to the schema. If
future work finds `os=*` insufficiently expressive, that is a separate, small follow-up
issue against `internal/runnercap`'s vocabulary — filed when it is actually needed, not
spent here.

## 8. Out of scope

- Cross-daemon stage distribution in the local scheduler (§2.3).
- Node provisioning automation for Windows worker pools.
- Windows container base-image engineering (persistent-VM provisioning is the only shape
  this document assumes, per §3's note on #651).
- BSDs / ARM-Windows (`cross-platform-support.md` §5) and portal work (already
  platform-neutral).
- Resolving P10 (#647) or P11 (#651) themselves — this document only marks the sections
  that depend on them as provisional.

## References

- `docs/design/cross-platform-support.md` §2, §3 (P13), §4
- `docs/design/v2-cloud-scale.md` (Workstream G2 — gaggle→task-queue partitioning)
- `docs/design/k8s-infra-shape.md` §7 (Windows node pool sizing, already anticipates P13)
- `docs/ARCHITECTURE.md` §3.1 (interim/local runner), §3.2 (V2 Temporal tier-3)
- `api/v1alpha1/workflow_types.go` (`Task.RequiredCapabilities`)
- `api/v1alpha1/gaggle_types.go` (`GaggleSpec.RequiredCapabilities`)
- `internal/instance/config.go` (`RunnerConfig.Capabilities`), `internal/instance/gagglecapability.go`
- `internal/runnercap` (capability vocabulary/matching)
- `internal/localscheduler/scheduler.go` (schedule-time fail-fast admission)
- Issues: #659 (this doc), #636/#639/#643 (closed inputs), #647/#651 (open, provisional inputs)
