# Mixed-Platform Cloud Nodes — Windows Node Pools & Platform-Labeled Routing

**Status:** Design-only (issue #659, P13 of `docs/design/cross-platform-support.md` §3).
**Approved for this design-doc scope only — no scheduler, provisioning, or Temporal
wiring is authorized by this document.** Implementation stays gated on a demonstrated
customer shape (§6 defines the trigger).

**Locked decisions (Lead ruling, 2026-07-25, recorded on #659):** the platform label is a
**stage-level** attribute, an unlabeled stage defaults to **linux**, and the scheduler
**fails fast** with a clear diagnostic when no node matches — no queue-and-wait.

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

This document holds that position exactly: **Linux pods remain the default and only
substrate most gaggles ever see.** Windows worker nodes are an opt-in pool that exists
only for stages whose toolchain genuinely requires Windows (Windows-only build systems,
.NET Framework, driver/desktop builds) — this is not a general multi-platform scheduler,
it is a narrow escape hatch for a substrate Goobers already treats as second-class outside
that one case.

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
enforced, is cheaper and matches "no implementation until demanded" — the doc reserves the
token, not a new contract surface.

- **Placement**: `Task.RequiredCapabilities` (`api/v1alpha1/workflow_types.go:174`) — a
  per-stage list, already unioned with the gaggle's own `RequiredCapabilities`
  (`api/v1alpha1/gaggle_types.go:38-51`) when a run is admitted
  (`internal/instance/gagglecapability.go:59-137` `WorkflowRequiredCapabilities`). This is
  exactly the "per stage" placement the Lead ruling locked — no schema change needed to
  reach it.
- **Who stamps it**: whoever authors the workflow definition (the same author who writes
  any other `RequiredCapabilities` entry, e.g. `dotnet@8`) — no new authoring surface, no
  separate approval path.
- **Default**: a stage with no `os=*` token in its resolved `RequiredCapabilities` is
  **unlabeled ⇒ linux**. Concretely: `internal/runnercap.Claimed.Missing` never receives
  an implicit `os=linux` requirement for an unlabeled stage — a Linux runner's claimed set
  need not (and should not) enumerate `os=linux` itself, since Linux is the ambient
  default, not an opt-in claim. Only `os=windows` (or a future non-Linux `os=*` value)
  is ever meaningfully "missing."
- **Reservation, not a new schema field**: nothing here requires a work-item contract
  change. If, once demand materializes, the implementation finds `os=*` too coarse (e.g.
  needing an architecture qualifier), that is a follow-up issue filed against
  `RequiredCapabilities`'s existing free-form vocabulary, not a reason to add a field now.

### 2.2 Mixed-platform pipelines: in scope, for free

Because the label is per-stage and the union happens per-run at admission time (not
per-workflow), a single workflow with stage A requiring nothing (linux) and stage B
requiring `os=windows` is **already representable and already correctly routed** by the
existing mechanism, once a Windows-capable runner exists to claim it — no additional
design work. **Decision: mixed-platform pipelines are in v1 of this design**, not deferred,
because "in v1" costs nothing beyond what P13's stage-level placement already implies. This
answers the open question in #659's body: routing is per-stage, and per-stage routing was
never blocked on a "whole pipeline" decision to begin with — the granularity issue does not
arise.

### 2.3 Interim (local) scheduler mapping

No mapping work is needed: `internal/localscheduler`'s existing schedule-time admission
check (§3 below) already treats `os=windows` as an ordinary capability token. The interim
scheduler has exactly one runner identity per daemon process (today's single-runner
model), so "Windows node pool" in the interim scheduler degenerates to: an operator runs a
second Goobers daemon process on a Windows host, whose `instance.yaml` claims
`os=windows`, pointed at the same gaggle config. Cross-daemon run distribution is out of
scope for the interim scheduler generally (single-runner model, `docs/ARCHITECTURE.md`
§3.1) and stays out of scope here — this document does not introduce multi-runner
dispatch to the local runner.

### 2.4 V2 (Temporal) task-queue mapping

`docs/design/v2-cloud-scale.md` Workstream G2 already partitions Tier 3 by mapping gaggles
to Temporal task queues + worker deployments per gaggle, so one hot gaggle cannot starve
others. Platform routing is an **orthogonal second axis on the same queue-naming scheme**,
not a competing model:

- Queue name becomes `<gaggle>` for the default (Linux) case — byte-identical to G2's
  existing scheme, so a gaggle that never uses `os=windows` sees no change at all — and
  `<gaggle>-windows` for a run whose resolved `RequiredCapabilities` include `os=windows`.
- A Windows worker deployment (§3) polls only `<gaggle>-windows`-suffixed queues for the
  gaggles it serves; it never claims the unsuffixed queue.
- Dispatch-time queue selection is a pure function of the same
  `WorkflowRequiredCapabilities` union the interim scheduler already computes — the V2
  runner does not re-derive platform routing from scratch, it reads the same admission
  facts through a different transport.
- This mapping is a **sketch for when V2 work starts**, not an implementation instruction;
  V2 Tier 3 itself is not yet built (`docs/ARCHITECTURE.md` §3.2 — Temporal is explicitly
  "never part of the product surface" today).

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

**Decision (Lead ruling): fail fast, not queue-and-wait.** This is not a new behavior to
build — it is the existing schedule-time admission check, reused as-is:

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

The code comment at that site already calls this "the load-bearing seam a future
dynamic/multi-runner router grows from" — this document confirms that a `os=windows`
capability is exactly such a case, requiring no new failure mode: a workflow with an
`os=windows` stage and no Windows-claiming runner in the fleet is refused to schedule with
a `missing capability: os=windows` diagnostic, exactly like any other unmet
`RequiredCapabilities` entry today. No timeout, no queue, no partial-run start.

**Observability**: the refusal is already a first-class journal event
(`journal.EventTickSkipped`) with a `Reason` string naming the missing token — this is
sufficient for the execution record to answer "why didn't this run start" without new
telemetry. When V2's per-gaggle/per-platform queue routing (§2.4) exists, the equivalent
observability is: a run's queue-selection decision (which queue name it was dispatched to,
and why) recorded alongside the run the same way today's `EventTickSkipped` reason is —
sketched here, not specified, since V2 dispatch itself does not exist yet.

## 6. Implementation trigger

Per `cross-platform-support.md` §3's own gate ("no implementation until a customer shape
demands it") and #659's scope, this section makes that trigger concrete rather than
leaving "customer demand" as an unfalsifiable placeholder:

**The trigger is: a specific gaggle, with a specific workflow, has at least one stage
whose build/test toolchain cannot run on Linux** (a genuine Windows-only dependency —
.NET Framework, a Windows-only build tool, driver/desktop code — not merely "the team
prefers Windows"), **and that gaggle is already active** (not hypothetical/prospective).
When that concrete case exists:

1. The gaggle's workflow author adds `os=windows` to the relevant stage's
   `RequiredCapabilities` (§2.1) — no schema change needed, this works today for the
   *authoring* half.
2. Provisioning a Windows node that claims `os=windows` in its `runner.capabilities`
   becomes the actual trigger for implementation work — until then, step 1 alone just
   produces a correctly-fail-fast-refused run (§5), which is itself proof the routing
   model requires no new machinery to express the requirement, only a node to satisfy it.
3. That provisioning need is what should be filed as the implementation issue(s), scoped
   against whichever of P9/P10/P11 are still open at that time (§3's provisional
   sections) — not against this design doc, which remains closed once merged.

No dates, capacity planning, or provisioning automation are proposed here — this section
exists solely so "customer demand" is recognizable when it arrives, per #659's own
acceptance criterion.

## 7. Contract reservations

None. §2.1 deliberately reuses the existing `RequiredCapabilities` free-form vocabulary
rather than reserving a new field — the "no implementation, but keep it cheap later"
goal is met by *not* adding to the schema. If future work (post-trigger) finds `os=*`
insufficiently expressive, that is a separate, small follow-up issue against
`internal/runnercap`'s vocabulary — filed when it is actually needed, not spent here.

## 8. Out of scope

- Any scheduler, queue, or Temporal implementation (§2.3, §2.4 are sketches only).
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
