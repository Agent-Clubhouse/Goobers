# Goobernetes v1 — architecture

Status: approved — Goobernetes v1 design. Encodes the PO decision record in
[goobernetes-decisions.md](goobernetes-decisions.md) (2026-08-22).
**Grounded against** `main` @ `21c645a6` and the spike-ladder evidence recorded on #2838.
Where this document and the decision record disagree, the record wins.

Goobernetes is the distributed execution mode of Goobers: every stage attempt of a run
executes in its own fresh container pod, placed by a constraint solve against a declared
runner inventory, with the daemon as the single control plane and the run journal — the
product output — written live and decoupled from the engine's internal history. One DSL,
one binary, one repo; the local runner is never replaced.

This is the top-level document of the Goobernetes v1 set. The decision record
([goobernetes-decisions.md](goobernetes-decisions.md)) is authoritative;
[distributed-state-and-coordination.md](distributed-state-and-coordination.md) (promoted from
the spike branch per record D5) governs state and coordination; companion documents cover
DSL 3.0 (record D2), the instance runner inventory and admission (D3/D4), data planes (D6),
restrictions (D7), images and scheduling (D8/D9), and the v1 exit smoke (D11). This document
owns the mode model, the substrate, the control plane, the end-to-end flow, and the explicit
supersessions of prior architecture text.

---

## 1. Decisions

Numbers below are this document's; the `record D#` column maps each to the PO decision
record entry it encodes.

| # | Decision | Record | Why |
| --- | --- | --- | --- |
| D1 | **Three modes, one DSL, one binary.** Local, cloud single-pod, Goobernetes (distributed). Mode is a property of the *instance*, never of a workflow document | D1 | The product is the DSL and the output (journal, PRs, provider effects); a workflow must not know or care where it runs. Tiers 1–2 remain first-class forever (v2-cloud-scale.md §10) |
| D2 | **Mode is inferred from inventory shape.** `runners:` (plural) supersedes the singular `runner:`; an instance with no `runners:` treats the legacy block as the implicit `self` entry. `engine:` stays optional Temporal connection config; a runner with a non-self host and no resolvable `engine:` is a validation error | D3 | Shipped behavior already infers local from `engine:` absence (`internal/instance/config.go`, `Engine *EngineConfig` — "Nil keeps the local daemon"). Requiring a mode field would break every deployed instance file, including the drifted live fork |
| D3 | **Pod-per-stage substrate.** Every mode-3 stage attempt executes in a fresh, never-reused pod the dispatcher creates for the resolved runner, disposed after its outputs are surrendered | D1 | Fresh-per-attempt is already doctrine (ARCHITECTURE.md §5, k8s-infra-shape.md §2 "never long-lived", #41, #666's single-use invariant); only the substrate was unbuilt — shipped tier 3 ran stages as goroutines in a resident `goobers worker` Deployment. This closes epic A3's gap on the proven per-activity routing seam |
| D4 | **Host kinds: `self` \| `image` \| `deployment`**, designed extensible (§3) | D3 | `self` keeps modes 1–2 (and any non-schedulable substrate, e.g. macOS) on the local execution path; `image` is the canonical product-rendered pod; `deployment` gives consumers full pod-spec control without surrendering the fresh-per-attempt lifecycle |
| D5 | **The daemon is the control plane.** Claims, provider quota, open-PR caps, and fairness stay daemon-owned. The tier-3 scheduler fork `internal/scheduler` is **deleted**; `cmd/goober-runtime` is **retired** (#2055 resolved: supersede) | D5 | `internal/scheduler` is a widening one-directional fork missing ~4 months of policy (#2055); `cmd/goober-runtime` is a superseded per-run pod runtime kept compiling as reference (`cmd/goober-runtime/main.go` banner). Three parallel schedulers/runtimes in-tree is proven drift risk (engine drift ledger #156) |
| D6 | **Daemon write API + live journal service.** Claim/release for ledger-touching stages, external trigger ingestion, HITL escalation resolution, and live journal event ingestion — one surface, co-designed with the claims store per distributed-state-and-coordination.md | D5 | "Designing them separately produces three coordinators." Today every external trigger and HITL gesture is a file drop into the daemon pod's `SchedulerDir`; the spike fired runs by `kubectl exec` into the daemon pod. That is not an operable product surface |
| D7 | **The journal is decoupled from Temporal history.** Stage activities emit journal events as they happen (idempotency keys for retried activities); one writer service owns sequence allocation. Live stage-transition visibility is a **v1 functional requirement**; per-token streaming is not | D5 | The closed-run-only projection model (`CompletedRunReconciler`, 30s loop over CLOSED runs; spans synthesized backdated at close, #2865) makes an in-flight distributed run invisible — stall detection, SSE, and the portal all key off journal liveness. The journal is the product; its availability cannot be a function of the engine's internals |
| D8 | **Temporal is retained, held loosely** behind the runner seam and the journal decoupling | D0 | Proven and fits the shape (durable retries, per-activity routing, deterministic replay). D7 removes its largest product coupling; if Temporal ever fights the shape it is revisitable without touching the DSL, the journal, or the pod substrate. Its determinism rules still bind: placement visible to the workflow function must be a pure function of declared inputs |
| D9 | **Task queues key on (gaggle × runner-type)** | D9 | #656's fairness/isolation contract survives; the platform-suffix scheme (`stageTaskQueue`/`platformQueueSuffix` in `internal/engine/engine.go`, `<workflow-queue>-<goos>`) is subsumed: OS becomes one input to runner-type resolution rather than the only routing axis |
| D10 | **Admission is three checkpoints**: apply-time constraint solve (error when a `runners:` inventory is declared, warning otherwise — fixes the #3497 severity trap for the declared case); bounded fail-fast at dispatch; boot **never** kills the daemon (implements #2860's decided-but-unimplemented ruling). Inventory updates are accept-and-pin | D4 | Apply-time truth rots (spot eviction, scaled-to-zero pools), so capability-unsatisfiable is an apply error while capacity-exhausted is a bounded, named runtime failure. `CheckCapabilityRequirements` still hard-fails the whole daemon at startup today — "refusing one run is proportionate; refusing to start is not" becomes real code in this work. Accept-and-pin also answers #3449: no drain machinery is required for config updates |
| D11 | **Windows stages run as container pods** | D1 | Resolves #651 in the container direction on the spike-ladder evidence (run `300534f6f9503e251374d9433060ebf8`, three stages across a Linux and a Windows node in one AKS cluster, one journal, #2838). Supersedes mixed-platform-cloud-nodes.md §3's provisional persistent-VM posture (§10) |
| D12 | **Two Windows facts are solver rules**: ledger-touching stages never place on Windows; Windows dispatch timeouts default higher, with diagnostics naming the cause | D9 | Windows pods can never mount the Linux node's RWO instance-root disk (#2842) — they structurally cannot reach instance state, so the solver encodes it rather than discovering it at runtime. Scale-from-zero Windows node provisioning plus multi-GB image pulls would otherwise present as spurious dispatch timeouts |
| D13 | **Runner claims are trusted in v1** (the RRQ-1 model): a false claim degrades to a runtime error with a named diagnostic. Probe-pod verification is a later honesty layer | D3 | The trust model is stated, not implied. The spike's own workaround ("claim `os=windows` on a Linux container and document the lie") shows both that trust is workable and that it needs the later probe layer |
| D14 | **Conformance surface unchanged.** No new conformance-normative journal event types for placement, pod lifecycle, pools, or caches. Placement provenance (runner name, node, OS, image, attempt) is journal-recorded under the **`runner.*` namespace** (non-normative) and surfaced via new `StageAttempt` read-model fields | D5 | ARCHITECTURE.md §3.3 sanctions exactly one runner-specific divergence channel: `runner.*`. The same workflow with fixed stage effects must produce equivalent normative journals in all three modes, or mode 3 has forked the product |
| D15 | **#3482 is increment 1** of Goobernetes, not a separate design | D1 | Moving execution out of the API pod poses precisely the mode-3 design questions (dispatch, workcopies, per-stage credentials, drain, journal ownership, migration). Answering them twice produces competing designs for one seam |
| D16 | **Zero deference to the shipped cloud topology** — the supersessions in §10 are deliberate | D0 | Continuity for the existing cloud deployment is a non-goal. Shipped tier-3 machinery carries forward only where it is the right design |

---

## 2. The three modes

One DSL, one repo, one binary. A workflow document never declares a mode; the instance's
runner inventory does (D2).

**Mode 1 — Local.** Unchanged. macOS/Linux/Windows hosts. macOS is *mode-1 only*: Azure
offers no macOS substrate, so an `os: macOS` stage on a cloud instance fails validation —
that is the system working as intended, not a gap. Nothing may break Mac local. With no
`runners:` block, the legacy singular `runner:` is the implicit `self` entry and behavior is
byte-identical to today — the zero-declaration compatibility rule every distributed feature
to date has honored (#2860, #2866, #662/#666, #656).

**Mode 2 — Cloud single-pod.** The existing daemon-in-a-pod shape
(`deploy/reference/goobers-system/api-deployment.yaml` running `goobers up` on an RWO
journal PVC). Its known defect — every run executes inside the API pod while the deployed
worker idles (#3482) — is fixed as increment 1 (§9), which moves stage execution behind the
same dispatcher seam mode 3 uses.

**Mode 3 — Goobernetes.** Every stage attempt executes in its own fresh, never-reused
container pod, disposed after its outputs are surrendered. Windows stages are container pods
in the cluster (D11). The instance root, daemon, and control-plane state remain
single-node/RWO exactly as k8s-infra-shape.md §4 requires — mode 3 distributes *stage
execution*, not the instance.

### Mode inference (D2)

- `runners:` absent → the singular `runner:` block is the implicit `self` entry; modes 1–2.
  Instance config gains a schema-version field as part of the pluralization (record D3);
  inventory changes are restart-only in v1.
- `runners:` present with only `host: self` entries → still local execution, multi-claim
  inventory (a validated superset of today).
- Any runner with `host: image` or `host: deployment` → distributed dispatch for stages the
  solver places on it. `engine:` must be resolvable (explicit or env, per
  `ResolveEngineConfig`) or validation fails. `engine:` itself remains what it is today:
  optional Temporal connection config, never a mode discriminator.

---

## 3. The substrate: how a stage acquires a fresh pod

The shipped tier-3 substrate is a resident `goobers worker` Deployment that executes stage
activities as goroutines (`deploy/reference/README.md`: the worker "does not create one pod
per stage"). That substrate is superseded (§10). In its place:

**The dispatcher** is the resident component that serves a runner's task queues. It does not
execute stages. Per stage activity it:

1. receives the activity from the (gaggle × runner-type) queue (D9);
2. creates one fresh pod for the resolved runner from its host kind (below), applying
   pod-level restrictions as the pod creator (record D7);
3. supervises the stage to completion, relaying liveness;
4. collects the `ResultEnvelope`, confirms output surrender (§5), and **disposes the pod**.
   A pod serves exactly one stage attempt, then is deleted — reuse is a correctness bug,
   not an optimization opportunity.

**Host kind semantics (D4):**

- `host: self` — the stage executes on the daemon's own host through the local execution
  path (fresh worktree per attempt, exactly `createStageWorkspace` semantics). No pod is
  created. This is modes 1–2, and the only legal host for substrates Kubernetes cannot
  schedule.
- `host: <image ref>` — the dispatcher renders the pod spec itself: the named image, the
  base runner contract (record D2: goobers binary, stage-contract environment, network to
  provider endpoints, credential delivery), resource *requests* from the stage's declared
  minimums and *limits* from the runner's declared ceiling, restrictions per record D7, and
  the deny-first NetworkPolicy posture of k8s-infra-shape.md §5. This is the canonical
  mode-3 runner; the product-owned minimal images (record D8: `goobers-base`,
  `goobers-harness-copilot`, `goobers-harness-claude`) exist to be named here.
- `host: <deployment name>` — the named Deployment is a consumer-authored **pod template by
  reference**, not a running pool: the dispatcher instantiates a fresh pod per stage attempt
  from that template (sidecars, volumes, node selectors under consumer control), and the
  fresh/never-reused lifecycle still holds. A Deployment that also runs resident replicas is
  not part of this contract. Template-extraction mechanics are an open point (§12).

The kind set is designed extensible (record D3) so later substrates (e.g. non-k8s backends,
#1837) are new kinds, not new schemas.

**What a pod must surrender before disposal** (the disposal gate): blobstore write-through
of artifacts and transcript spans (`internal/blobstore`, `workerhost.StagingArtifacts` —
content-addressed, idempotent by digest, shipped and proven cross-OS by #2866); its journal
events through the write API (§4); its `ResultEnvelope` into the engine. There is no
"collect the journal from the pod": the journal is written live, and pod-local state is by
definition disposable.

---

## 4. The control plane: the daemon

The daemon is the single control plane (D5). What it owns does not move: the claims ledger
(`internal/localscheduler/claim.go` — explicitly designed for one embedded scheduler per
instance), provider quota, open-PR caps, fairness, auth circuits, trigger evaluation. Mode 3
does not distribute this state; it gives remote stages a governed way to reach it.

**Deleted and retired (D5).** `internal/scheduler` — the quarantined tier-3 scheduler fork —
is deleted; its only production-shaped consumers (`goobers engine-start`, deterministic
RunID dispatch) are re-homed on the daemon's dispatch path. `cmd/goober-runtime` is removed;
#2055 is resolved as *supersede*, citing this document. The localscheduler admission site's
own comment ("the load-bearing seam a future dynamic/multi-runner router grows from") is the
growth point; there is exactly one scheduler after this work.

**The daemon write API (D6).** The v1 surface, co-designed with the claims store per
distributed-state-and-coordination.md:

- **claim/release** for ledger-touching stages: backlog-query, close-out, release-claim run
  wherever the solver places them (never Windows, D12) and mutate the ledger only through
  this API — the per-node divergent-claims.json failure recorded on #2838 becomes
  unrepresentable;
- **external trigger ingestion** — replaces the `pending-triggers` file-drop; a mode-3 run
  is startable without exec access to the daemon pod;
- **HITL escalation resolution** — humans resolve gates through the API/portal, not the
  filesystem;
- **live journal ingestion** — below.

**The live journal service (D7).** Stage activities emit journal events as they happen,
carrying idempotency keys so a retried activity cannot double-append. One writer service —
the daemon-side successor of the single-writer discipline `InstanceLog.Append` and the
per-run journal flock implement today — owns sequence allocation. Consequences:

- the journal is **decoupled from Temporal history**: history stops being the journal's
  source of truth, and the closed-run-only projection model dies (§10);
- runs are **live**: stall detection (`StalledRunTimeout`), SSE, and the portal work
  mid-run. Live stage-transition visibility is a v1 functional requirement; per-token
  streaming is not;
- journal storage stays on the daemon's RWO volume with a single writer — the
  k8s-infra-shape.md §4 constraint is honored by construction rather than worked around.

---

## 5. Scheduling flow, end to end

The full path of one run in mode 3:

1. **Trigger.** Cron (daemon schedule evaluation), webhook, or an external caller through
   the write API (D6).
2. **Daemon admission.** Claims, budgets, provider quota, open-PR caps, fairness — the
   existing localscheduler policy, unforked (D5). The workflow was already constraint-solved
   at apply time (D10 checkpoint 1); admission re-checks nothing the solver settled.
3. **Dispatch.** The daemon starts the engine workflow. Each stage activity is routed to a
   **constraint-resolved queue** keyed (gaggle × runner-type) (D9): the solver maps the
   stage's `runsOn` (os, quantities, capabilities, restrictions — record D2/D7) plus derived
   requirements onto a runner type from the inventory. Placement is a pure function of
   declared inputs, keeping the workflow function deterministic (D8).
4. **Reality backstop.** A bounded schedule-to-start class timeout remains (D10
   checkpoint 2): capability-unsatisfiable was already an apply-time error; a queue no
   dispatcher serves, or an exhausted pool, is a bounded, named runtime failure — higher
   default bounds on Windows, with the cause named (D12).
5. **Pod.** The dispatcher creates the fresh pod (§3), which provisions its workspace: fresh
   worktree on the run branch fetched from origin at a declared handoff edge (record D6),
   artifacts materialized from the blobstore by digest, credentials delivered per the
   #2931 references-only contract (values re-resolved at execution, never in history).
6. **Execution.** The stage runs; journal events stream through the write API (D7);
   ledger-touching stages call claim/release (D6).
7. **Surrender.** Artifacts and spans write through to the blobstore; a repo-writing stage
   with a declared handoff pushes the run branch (`goobers/<workflow>/<run-id>`) to origin
   (record D6 — modes 1/2 unchanged, no push); the `ResultEnvelope` returns.
8. **Disposal.** The dispatcher deletes the pod. A pod or node killed mid-stage classifies
   as an **infra attempt** (non-normative, retried on the infra budget with a fresh pod —
   #2878/#3361); a gate-ordered repass is a **policy attempt** on a fresh pod on whatever
   node the solver picks, with the run branch and verdict pointer carrying the state.

Warm pools are deferred (record D9); v1 is cold-start-correct, with stage `timeoutSeconds`
defaults resized so cold paths degrade to slow, never to spurious timeout failures
(record D6).

---

## 6. Windows

Committed, not provisional (D11): Windows stages run as container pods on Azure Linux +
Windows Server 2022 targets (record D8), which explicitly promotes windows/amd64 from
TierExperimental (stated in the support matrix, not implied). The evidence bar was the spike
ladder: a Windows agentic run produced a merged PR in 2m25s, and the mixed-OS run spanned
both node OSes under one journal (#2838).

The solver carries the two structural facts (D12) rather than letting operators rediscover
them: ledger-touching stages never place on Windows (no path to the RWO instance root —
#2842 — and none needed once claims ride the write API); Windows dispatch bounds default
higher to absorb scale-from-zero node provisioning and multi-GB pulls, with diagnostics that
name which cost was being paid. Windows *sandboxing* is its own epic (record D7):
restrictions are Linux-pod-enforced in v1, and a stage requiring a restriction can only
match runners that enforce it — so an unrestricted Windows runner is simply unmatchable for
restricted stages, visible at apply time.

---

## 7. Conformance and provenance

The conformance invariant is untouched (D14): the same workflow with fixed stage effects
produces equivalent normative journals in all three modes — the ordered orchestration event
set of ARCHITECTURE.md §3.3, policy attempts normative, infra attempts excluded. Mode 3 adds
**zero** conformance-normative event types; pod lifecycle, queue residency, pool state, and
cache behavior are telemetry and `runner.*` facts, never conformance surface (the same rule
external-call-out-stages.md D14 enforces for its domain).

Placement provenance — runner name, node, OS, image, attempt — is journal-recorded under
`runner.*` (the sanctioned divergence namespace) and surfaced through new `StageAttempt`
read-model fields, which today carry no placement identity at all
(`internal/readservice/runs.go`, `StageAttempt`). Provenance is thus authoritative
(journal-recorded, survives telemetry outage) without being normative (a local run's journal
remains conformant with none of it).

---

## 8. Temporal posture

Retained, held loosely (D8, record D0). What Temporal provides — durable per-activity
dispatch, deterministic replay, infra-retry orchestration, long waits — is exactly the shape
of the problem, and the engine's activity seam (`classifySeamError`, `dispatchWithRetry`,
per-activity `TaskQueue`) carries forward. What changes is the coupling: with the journal
decoupled (D7) and the scheduler unforked (D5), Temporal's blast radius shrinks to "the
dispatch fabric between daemon and dispatcher." Raw Temporal mechanics stay off the product
surface (ARCHITECTURE.md §3.2); replacing Temporal, should it ever fight the shape, would
strand no product contract — that is the definition of held loosely, and it is a design
constraint on every new interface in this set: nothing product-visible may name Temporal.

---

## 9. Increment 1: #3482

The first shippable increment is moving execution out of the API pod (D15): the daemon stops
running stages in-process and dispatches through the same seam the mode-3 dispatcher serves,
against a single implicit runner. This forces, at the smallest useful scale, every hard
question in this document — dispatch wiring, workcopy locality, per-stage credential
delivery across pods, drain/restart semantics (replacing the in-process WaitGroup drain),
and journal single-writer ownership — while remaining a mode-2 deployment a current adopter
can run. Migration is dual-topology: an instance upgrade must not require simultaneous
daemon+worker cutover.

Sequencing within v1 follows the recorded rule (distributed-state-and-coordination.md,
restated on #3278): the write API and claims store land together, before scheduling policy
that would otherwise be built twice.

---

## 10. Supersedes

Record D0's zero-deference ruling, applied. Each row names what dies; the cited documents
gain a banner pointing here when this design is approved.

| Prior text | What dies | Replacement |
| --- | --- | --- |
| docs/ARCHITECTURE.md §3.2 ("Temporal history is the internal durability mechanism… projects history down into the run-journal format") | The **closed-run-only projection model** as the journaling architecture: `CompletedRunReconciler` + `startEngineProjection` (30s loop over CLOSED runs) and close-time backdated span synthesis (`SynthesizeRunSpans`, #2865) as the only visibility path | Live journal service (D7): events written as they happen through the daemon write API; Temporal history is engine-internal state, not the journal's source. Whether `ProjectRun` survives as a recovery/verification tool is an open point (§12) |
| docs/ARCHITECTURE.md §1/§3 tier vocabulary | "Tier 3" as the name of the distributed mode, and the implication that the deployment-tier table is the mode model | The three-mode model (§2); deployment tiers remain a packaging concept, modes are the execution concept. ARCHITECTURE.md is amended, not rewritten — the runner seam and conformance property (§3.3) stand |
| docs/design/v2-cloud-scale.md §2 (A1.5/A1.6), worker posture throughout | **Persistent-worker-as-substrate**: `goobers worker` Deployments executing stage activities as resident goroutines as the target execution model; A1.5's history→journal projection as the journaling end-state | Pod-per-stage under a dispatcher (§3); live journal (D7). The foundations the branch delivered (conformance harness, blobstore, workspace provisioner seam, reference manifests) carry forward — the *resident execution* posture does not |
| docs/design/v2-cloud-scale.md §8 G2 / #666 pool keying | Per-gaggle warm-pool keying as the recorded pool design | Warm pools stay deferred; the recorded constraint for their later design is pools shareable across gaggles per runner type (record D9, superseding #666's keying), with the pre-bind identity question recorded open |
| docs/design/mixed-platform-cloud-nodes.md §3 | The **persistent-Windows-VM posture** ("Windows workers = persistent VMs, provisional pending #651") | Windows container pods (D11); #651 is resolved in the container direction on spike evidence |
| docs/design/mixed-platform-cloud-nodes.md §2.1–§2.2 | The locked #659 ruling ("platform is a token, not a field") and the `<workflow-queue>-<goos>` routing scheme as the platform mechanism; the unlabeled⇒linux *semantic* | `runsOn.os` as a validated field with `os=*` tokens rejected in 3.0 (record D2 — the drift hazard #659 guarded against is closed by making the vocabularies unable to coexist); (gaggle × runner-type) queues (D9). The unlabeled default becomes explicit-complete: an os-unspecified stage has no OS requirement, and *placement policy* — ours to own — prefers and will wait for a Linux-class runner when the inventory has one (record D2), preserving the observed behavior of today's workflows without the hidden semantic |
| #2055 (sync/supersede/delete decision) | `internal/scheduler` and `cmd/goober-runtime` | Deleted/retired (D5); #2055 closes as supersede citing this document |

Not superseded, and load-bearing here: the stage contract's pointer-only boundary and fresh
workspace rule (docs/stage-contract.md), the conformance relation (ARCHITECTURE.md §3.3),
k8s-infra-shape.md's RWO instance-root constraint and deny-first networking, the #2931
references-only credential ruling, and #2861's declared-handoff principle (generalized
cross-OS → cross-runner by record D6, not reversed).

---

## 11. Acceptance criteria

Falsifiable; each names its observer.

1. **Zero-declaration invariance.** An instance with no `runners:` block behaves
   byte-identically to today: same validation output, same journals, same scheduling. Guard:
   existing golden/conformance suites pass unmodified on such a config.
2. **Conformance across modes.** The A2 dual-runner harness, extended to mode 3, shows an
   empty normative-event diff for the smoke workflows; all placement provenance appears only
   under `runner.*`.
3. **One scheduler.** `internal/scheduler` and `cmd/goober-runtime` are absent from the
   tree; `grep` finds no production caller of the deleted seams; #2055 is closed as
   superseded.
4. **No exec-only operations.** The v1 exit smoke (record D11) fires an external trigger and
   resolves a HITL escalation **without** `kubectl exec` into any pod, through the write API
   alone.
5. **Live visibility.** During the smoke, the portal shows a stage transition for a running
   distributed run within the SSE path's normal latency — not at run close.
6. **Fresh-pod invariant.** In the smoke, every stage attempt's `runner.*` provenance names
   a distinct pod; pod-kill and node-kill during each stage class journal as infra attempts
   and the run completes; a repass executes on a fresh pod with the prior commits and the
   gate verdict present.
7. **Windows facts hold.** A ledger-touching stage is never observed on a Windows runner (
   solver refusal is journaled/diagnosable); a Windows scale-from-zero dispatch that exceeds
   the Linux default bound produces the cause-naming diagnostic, not a generic timeout.
8. **Boot proportionality.** A config declaring a requirement no runner claims fails at
   apply/validate with an error (inventory declared) and — for the undeclared legacy case —
   refuses the affected run at admission without killing the daemon (#2860 implemented; the
   current `CheckCapabilityRequirements` boot-kill path is gone).

The volume scale test is explicitly **not** v1 exit (record D11); its candidate targets live
in the smoke document as the next rung, gated on the #2871 parity items (notably #2875
cumulative budgets).

---

## 12. Open implementation points

Recorded deliberately; none re-opens a decision.

1. **Dispatcher topology.** One dispatcher per runner-type vs. a consolidated dispatcher per
   instance; where it runs (goobers-system vs. per-gaggle namespace); how it maps Temporal
   activity delivery onto pod creation without fighting the SDK's poller model.
2. **`host: deployment` template extraction.** The exact contract for reading a pod template
   from a named Deployment (replicas expectations, annotation for opting in, validation of
   template compatibility with the base runner contract).
3. **Fate of `ProjectRun`/`CompletedRunReconciler`.** Deleted with the projection model, or
   retained as a recovery/backfill/verification tool against engine history.
4. **Journal writer failover.** Single-writer ownership handoff on daemon restart mid-run;
   idempotency-key retention window; write-API backpressure semantics for a chatty stage.
5. **Claim-lease renewal source.** Whether renewal keys off engine workflow liveness rather
   than daemon in-process run tracking, so a daemon restart cannot lapse a live distributed
   run's claims (today's 30-minute daemon-renewed lease).
6. **Write API transport and authn.** Protocol, in-cluster service shape, and how a stage
   pod authenticates (per-gaggle workload identity vs. minted per-attempt token) within the
   #2931 references-only rule.
7. **Warm-pool pre-bind identity** (record D9's recorded open point): what
   identity/credentials a pre-warmed shared pod may hold before a gaggle claims it.
8. **#3482 migration mechanics.** Dual-topology window, feature-flag or inventory-driven
   cutover, and rollback for a mode-2 instance adopting the dispatcher seam.
9. **Engine ceilings.** Temporal history/payload budgets under pod-per-stage with
   repass-heavy runs at smoke scale, and self-hosted Temporal+Postgres sizing guidance.
10. **ARCHITECTURE.md amendment mechanics.** The concrete edit set for §1/§3 (banner +
    section rewrites) lands with this design's approval PR, per the supersessions table.

---

## 13. Issue cross-references

Per record D12: epic **#2889 remains the umbrella**, re-scoped to this design. Resolved by
this document's decisions: #2055 (delete/supersede), #651 (container direction), #3482
(increment 1), #2860 (implement), #659 (formally superseded via record D2). Folded:
#656 (D9), #2861 (generalized, record D6), #2898/#1307 (record D7), #2901/#644/#2912
(record D10), #3275 (scoped per record D8), #3276. Deferred with rationale recorded:
#2051/#2053 (coordination beyond the write API's v1 scope), #662/#666 (warm pools, D9
constraint recorded). New sub-epics: Windows sandboxing; deferred warm pools; deferred
volume scale.
