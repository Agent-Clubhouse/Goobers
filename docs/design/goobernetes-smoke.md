# Goobernetes smoke — the distributed-shape v1 exit

Status: approved — Goobernetes v1 design. Encodes the PO decision record in
goobernetes-decisions.md (2026-08-22).

This document specifies the **v1 exit criterion for Goobernetes** (decision record **D11**): a
**distributed-shape smoke, not a scale test**. It fixes the topology, the falsifiable acceptance
criteria — each with a *named observer*, the event or record that proves it, in the style #3278
established ("a stranger with an empty cluster … reaches a merged pull request"; any hand edit is
filed as a defect) — and the procedure rules. It then captures the **deferred volume-scale rung**
explicitly: its candidate targets, its gating parity items (#2871, notably #2875), and its harness
pointers, so the next rung is a recorded plan rather than a re-litigation.

**What this is not.** It is not a performance measurement, a soak, or a capacity plan — those are
the deferred rung (§6). It is also not a re-proof of what the spike ladder already recorded: the
#2838 rung table has an OS-spanning run GREEN, Spike 1 AC2 recovered a worker-pod delete and a node
restart mid-agentic-stage on the infra-retry budget, and Spike 2 AC2 recovered a spot eviction
mid-stage. The smoke *extends* those proofs to the flows the spikes never exercised: fresh-runner
repass across nodes, declared-edge repo handoff across nodes, operation entirely through the daemon
write API, live mid-run visibility, and a restriction proven by a negative control.

---

## 1. Decisions

| # | Decision | Why |
| --- | --- | --- |
| D1 | The v1 exit is a **shape smoke**: every distributed behavior demonstrated once, at trivial volume | D11 verbatim. Volume adds no information about *shape* correctness, and the open #2871 parity gaps (notably #2875 cumulative budgets, unenforced on the engine path) mean a volume run today would burn agentic spend invisibly — §6 gates the scale rung on closing them |
| D2 | Every criterion names its **observer** — the journal event, read-model record, API response, or portal observation that proves it | A criterion without an observer is a wish. #3278 set the precedent (falsifiable exit; hand edits are defects). Where the observer machinery is itself a v1 deliverable (placement events under `runner.*`, StageAttempt placement fields — decision record D5), the smoke *consumes* it and thereby proves it exists |
| D3 | The entire procedure runs through the **product surface**: DSL apply, the daemon write API, the portal, `goobers` CLI. **`kubectl exec` appearing anywhere in the procedure is a defect**, filed like #3278's hand edits | Every spike run was fired by `kubectl exec` into the daemon pod and worked *only* for that reason (pending-triggers file drop). The write API (decision record D5) exists precisely to remove that; a smoke that bypasses it proves nothing about it |
| D4 | Pass/fail follows the **e2e-soak-harness disciplines**: three-way pass / fail / **invalid** (a run that did not exercise what it claims is invalid, never a pass), evidence bundle captured on fail *and* invalid before teardown | [e2e-soak-harness.md](e2e-soak-harness.md) §7–§9 already argued this; a second vocabulary for the same contract is drift. "Nothing broke" must never be presentable when nothing ran |
| D5 | Kill-injection is the **only** permitted out-of-band cluster action, performed between observers, and each injection is itself recorded in the evidence bundle | The smoke must distinguish "the system recovered from a kill" from "no kill landed". An unrecorded injection makes S6 unfalsifiable |
| D6 | The scale rung's targets are **captured as candidates, not commitments**; they bind only when the rung's gate (§6.2) is closed and the numbers are re-affirmed then | D11: "candidate targets … are captured in the smoke doc as the explicit next rung." Numbers chosen before #2875-class enforcement exists would be calibrated against a system that cannot yet account for them |

---

## 2. Topology

Per D11: **~2 Linux nodes + 2 Windows nodes, ~6 stage pods total** over the smoke workflow's run.
Concretely:

- A customer-managed AKS-class cluster (documentation-first posture, #41 /
  [k8s-infra-shape.md](k8s-infra-shape.md)): 2 Linux nodes, 2 Windows Server 2022 nodes. Windows
  stages run as **container pods** (decision record D1; supersedes the provisional persistent-VM
  posture in [mixed-platform-cloud-nodes.md](mixed-platform-cloud-nodes.md) §3, on the spike-ladder
  evidence; resolves #651 in the container direction).
- Images per decision record D8: `goobers-base`, one harness image, **and one consumer-layered
  toolchain image** (carrying `local-ci`'s go/make needs) — tags = binary version (#3275). The
  minimal official set is itself part of the point (D8: the base image is v1-functional), and the
  consumer layer exercises the layering contract as a contract rather than a courtesy
  (deployment doc §8).
- The daemon (control plane, decision record D5) runs in-cluster; the smoke is also increment 1's
  exit in miniature — no stage executes in the API pod (#3482).
- **One Linux runner in the inventory declares `restrictions: [network:allowlist]`** and is the
  subject of S9. macOS does not appear: it is mode-1 only (decision record D1) and is out of scope
  here by construction, not omission.

Six-ish pods means: one run of the smoke workflow produces at minimum — a builtin stage pod
(Linux, `goobers-base`), an agentic implement pod (Linux, harness image), a `local-ci` pod
(Linux, different node, the consumer-layered toolchain image), a Windows stage pod, a repass pod
(fresh, S5), and the allowlist-restricted probe pod (S9). Kill-injection retries (S6) add
attempts beyond the six.

---

## 3. Preconditions (observer machinery)

The smoke cannot run before these v1 deliverables exist; it is their first end-to-end consumer:

1. **Placement provenance in the journal** — `runner.*` events (runner name, node, OS, image,
   attempt; non-conformance-normative) and the corresponding new StageAttempt read-model fields
   (decision record D5). Today `StageAttempt` carries no placement at all
   (internal/readservice/runs.go:422 — id/visit/number/class/status/model, nothing about pod, node,
   or OS), so S1–S6's observers are new fields the smoke forces into existence.
2. **The daemon write API** — trigger ingestion, HITL escalation resolution, claim/release
   (decision record D5, co-designed with the claims store per
   [distributed-state-and-coordination.md](distributed-state-and-coordination.md)). S7's observer.
3. **The live journal service** — stage activities emit journal events as they happen; SSE and the
   portal work mid-run (decision record D5: live stage-transition visibility is a v1 functional
   requirement). S8's observer.
4. **Declared handoff edges in DSL 3.0** — the compiler rejects undeclared repo chains
   (decision record D6, generalizing #2861); the smoke workflow declares its implement→local-ci
   edge. S3 exercises the runtime half (push at the boundary, fetch in the next stage's fresh
   worktree).

---

## 4. Acceptance criteria

Each criterion is falsifiable and names its observer. All must pass in **one procedure** (a small
number of runs on one cluster, one evidence bundle); cherry-picking passes across rebuilt clusters
is a fail. Attempt-class assertions follow the #3361 contract: infrastructure faults journal as
`attemptClass: infra` on the bounded infra budget, conformance-excluded, never charging the work
budget (docs/stage-contract.md:751–756; #2840 AC2, #2878).

### S1 — Fresh pod per stage attempt, never reused

Every stage attempt of every run — including repasses (S5) and infra retries (S6) — executes in a
pod created for that attempt and disposed after its outputs are surrendered. No pod identity
appears under two attempts.

**Observer:** the run's `runner.*` journal events and StageAttempt placement fields. The check is
mechanical: project (attempt → pod identity) over the whole run; the mapping is injective, and
every pod named there is gone from the cluster after run close. A duplicate pod identity anywhere
— including "the repass reused the implement pod" — is a fail.

### S2 — OS hop within one run

A single run executes at least one stage on a Linux node and at least one on a Windows node, and
completes.

**Observer:** `runner.*` events for the same run ID showing `os: linux` and `os: windows` attempts;
the run's terminal journal event shows success. (The spike ladder proved OS-spanning routing GREEN
— #2838; what S2 adds is the same proof under the v1 machinery: `runsOn.os`, container Windows
pods, official images.)

### S3 — Declared-edge repo handoff across nodes

The implement→local-ci chain runs its two stages on **different nodes**, with the edge declared in
the DSL. At the boundary the runtime pushes the run branch (`goobers/<workflow>/<run-id>`) to
origin; local-ci's fresh worktree fetches it and finds implement's commits.

**Observer:** three records — (a) the declared edge in the applied workflow; (b) `runner.*` events
showing distinct node names for the two stages; (c) local-ci's journal shows it operated on the
implement commit (the branch head SHA recorded at push equals the SHA local-ci's worktree checked
out). A local-ci pass that cannot show it saw implement's commits is a fail — the silent-worktree
continuity assumption is exactly what this criterion exists to falsify.

### S4 — Artifact materialization across nodes

A stage on node A records an artifact (write-through to the blobstore before pod disposal); a later
stage on node B declares it as input and receives it (materialize-before-stage).

**Observer:** the `artifact.recorded` journal event (digest, producing attempt) on node A's
attempt, and node B's attempt journal showing the same digest materialized before stage start. A
missing blob must fail soft so the executor's integrity check classifies it (#2866) — a hard crash
on a missing digest is a fail of the classification contract even if retry recovers.

### S5 — Repass across nodes with the gate verdict pointer

A gate fails a stage; the repass attempt runs in a **fresh pod on a different node** than the
original attempt, and receives the gate's just-recorded Verdict as the `<gate>.verdict`
contextPointer (docs/stage-contract.md:865–874, #412) plus the prior commits on the run branch.

**Observer:** the repass attempt's journal — attemptClass **policy** (a repass is work, not
weather), `runner.*` node differing from the first attempt's, and the bound `<gate>.verdict`
pointer in its recorded inputs. This is the single least-proven flow in the system: today's repass
loop assumes workspace continuity on one node. A repass that succeeds without the verdict pointer
present is a fail (it "worked" by losing the gate's context).

### S6 — Kill matrix: pod-kill and node-kill, per stage class

Six injections, each recorded per D5: **pod-kill** and **node-kill** during (a) a builtin stage,
(b) an agentic stage, (c) local-ci. After each, the attempt journals as `attemptClass: infra`, the
retry runs in a fresh pod (S1), and **the run completes successfully**.

**Observer:** per injection — the evidence bundle's injection record (what was killed, when), the
interrupted attempt's `attemptClass: infra` journal entry with a typed `infra*` error class, the
successor attempt's fresh `runner.*` identity, and the run's successful terminal event. Two
explicit fail conditions: an interrupted attempt classified as a policy/work failure (charging
`Task.Retry` or the failure-streak breaker — the #3361 regression class), or a run that never
completes. Conformance check: the infra attempts are absent from the conformance view
(infra retries are non-normative — v2-cloud-scale §2 A1.5/A2).

### S7 — Triggers and HITL through the write API

The smoke's runs are **triggered** through the daemon write API (or a provider webhook landing on
the single ingress — never a file drop, never `kubectl exec`), and one run escalates to a human
whose **resolution** is submitted through the write API and unblocks the run.

**Observer:** the write API's journaled trigger-ingestion event carrying the run it admitted, and
the escalation's resolution event carrying the API-submitted decision, followed by the run
proceeding. Per D3, the procedure transcript itself is an observer: it contains no `kubectl exec`.
This criterion is what retires the spike posture ("every spike run was fired by kubectl exec into
the daemon pod").

### S8 — Live stage-transition visibility mid-run

While a multi-stage run is in flight, the portal shows its stage transitions as they happen —
before the run closes.

**Observer:** a recorded portal/SSE observation (timestamped screenshot or SSE event capture) of a
stage transition, with the run's terminal journal event timestamped **later**. Terminal-only
visibility — today's closed-run projection shape — is an explicit fail: live visibility is a v1
functional requirement (decision record D5). Per-token streaming is not asserted.

### S9 — `network:allowlist` proven by a negative control

The restricted Linux runner (§2) runs a stage that attempts egress to a non-allowlisted endpoint.
The attempt is **denied**, and the denial is distinguishable from a DNS failure — per the #2898
acceptance criteria, which this criterion adopts verbatim. Enforcement is CIDR-NetworkPolicy-backed
per the standing #2898 PO ruling (2026-08-16); the proxy/FQDN layer is #1307, later
(decision record D7).

**Observer:** the probe stage's recorded failure showing a connection-level denial (connect
timeout/refused to a resolved address), not `NXDOMAIN`/resolution failure; plus a positive control
in the same run — an allowlisted endpoint reachable from the same pod — so "denied" is not
"network broken". A `doctor --k8s` PASS is explicitly **not** an observer: today's
`checkNetworkPolicySupport` is API-discovery only (internal/k8spreflight/checks.go:62) and passes
on clusters with zero enforcement — the exact trap #2898 records. Only the in-cluster negative
control counts.

---

## 5. Procedure rules

1. **No `kubectl exec`, no hand edits** (D3). Either appearing in the transcript is a filed defect,
   #3278-style, and the smoke is re-run after the fix.
2. **Invalid ≠ pass** (D4): a criterion whose observer machinery failed (SSE capture lost, journal
   unreadable) is *invalid* and blocks the exit — it is never counted as passed, never as failed.
3. **Evidence bundle** on completion regardless of outcome: applied DSL + inventory, all run
   journals (`events.jsonl`), the injection records (D5), the write-API request log for S7, the S8
   capture, the S9 probe output, image tags and binary version, cluster/node inventory.
4. The smoke is **re-runnable from the bundle alone**: the same collateral on a rebuilt cluster
   reproduces the procedure. (This is the #3278 rebuild discipline applied to the smoke.)

---

## 6. The deferred rung: volume scale

**Deferred by decision** (D11: "the volume scale test is deferred"). This section captures the
rung so it is a plan on record, not a fresh design later.

### 6.1 Candidate targets (captured, not committed — D6)

| Axis | Candidate target |
| --- | --- |
| Concurrency | **~25 concurrent runs** held steady (ramp-then-hold, per e2e-soak-harness §5) |
| Duration | **24h soak** at that concurrency |
| Dispatch latency, warm | ceiling TBD — trigger→stage-start with a node available; candidate anchor: Spike 0 measured trigger→merged-PR 2m50s cold-cluster, so a warm per-stage dispatch ceiling well under a minute is the shape to beat |
| Dispatch latency, cold | separate, higher ceiling TBD — absorbing scale-from-zero node provisioning and multi-GB Windows pulls (decision record D9: Windows dispatch timeouts default higher, diagnostics name the cause; decision record D6: cold degrades to slow, never to spurious timeout) |
| Repass volume | a nonzero measured repass count across nodes (repass is the least-exercised flow; a soak with zero repasses did not test it) |

Numbers are re-affirmed against measured smoke-era baselines when the gate below closes; they are
deliberately not binding today.

### 6.2 Gate: the #2871 parity items

The scale rung **must not start** before the open #2871 parity-ledger items are closed on the
engine path, because a volume run under the gaps produces unaccounted spend and misclassified
failures:

- **#2875 — cumulative agentic usage budgets** (named by D11): unenforced on the engine path today.
  25 concurrent agentic runs for 24h with no cumulative budget enforcement **burns real spend
  invisibly** — this is the gate's headline item, and the reason the rung is gated rather than
  merely sequenced.
- **#2878** — transient provisioning failures as infra attempts (the smoke's S6 exercises the
  classification once; the soak needs it correct at volume or infra weather reads as product
  failure).
- **#2874** — non-retryable agentic failures route to escalation.
- **#2873** — history-to-journal conformance gaps.

### 6.3 Harness pointers

- **[e2e-soak-harness.md](e2e-soak-harness.md)** is the governing discipline for the soak shape:
  admission ramp/hold and stable reason strings (§5), named checked-in profiles (§6), the
  pass/fail/**invalid** three-way split (§7–§8), evidence-on-failure incl. automatic diagnostics
  capture (§9), never-blocks-PR-CI cadence (§11). The scale rung extends its driver from
  "N runs against one local daemon in one container" to "N runs against the cluster" — the
  contracts carry over; the isolation substrate changes.
- **B0 — the benchmark harness** (v2-cloud-scale §3, Workstream B) gates every cache/warm-pool
  optimization layer (B1–B5, warm pods). Dispatch-latency ceilings measured here are B0's
  baseline: warm pools (deferred, decision record D9) are justified by these numbers or not at all.
- Per-stage dispatch latency needs an emitter: today per-stage telemetry arrives only at run close.
  The `runner.*` placement events (§3.1) are the natural carrier for queue-wait/dispatch
  timestamps; the rung's design confirms this rather than inventing a parallel channel.

### 6.4 Explicitly out of the rung

Warm-pool policy design (deferred with its own recorded constraint, decision record D9), Windows
sandboxing (own epic, decision record D7), macOS-in-cluster (does not exist, decision record D1),
managed-SaaS topology (v2-cloud-scale §10 walls).

---

## 7. Issue cross-references

| Issue | Relationship |
| --- | --- |
| #2889 | Umbrella epic; this doc is D11's specification within it |
| #3278 | Falsifiable-exit precedent (observer discipline, hand-edit-is-a-defect rule) |
| #2838 | Spike ladder — recorded proofs the smoke extends, not re-runs |
| #3482 | Increment 1 (execution out of the API pod); §2 makes the smoke its exit-in-miniature |
| #2861 | Declared handoff edges, generalized cross-runner (S3) |
| #3361 / #2840 / #2878 | Infra attempt classification the kill matrix asserts (S6) |
| #2898 / #1307 | Negative-control acceptance adopted by S9; NetworkPolicy now, proxy/FQDN later |
| #3301 | Rendered-together CI assertion for the reference manifests S9's runner ships with |
| #651 / #3275 | Windows container pods and the published images the topology stands on |
| #2871 / #2875 / #2874 / #2873 | Parity-ledger gate for the scale rung (§6.2) |
| #2912 | Run ID = trace ID; the evidence bundle's runs are trace-navigable |
| #3276 | Worker scaling signal — a scale-rung dependency, not a smoke one |

## 8. Open implementation points

Recorded, not decided here:

1. **Exact latency ceilings** for the scale rung (warm and cold) — numbers bind only at gate-close
   (D6), calibrated against smoke-era measured baselines.
2. **Kill-injection mechanics** — chaos tool vs. scripted `delete pod` / node drain; D5 requires
   only that each injection is recorded. (Node-kill on a 2-node Windows pool implies capacity for
   rescheduling — whether the smoke cluster briefly scales a third Windows node or accepts a
   longer cold retry is a procedure detail.)
3. **S8 capture form** — SSE event log vs. timestamped portal screenshot; either satisfies the
   observer, the procedure should pick one and automate it.
4. **Negative-control delivery** — whether the S9 probe is a smoke-workflow stage, a doctor-driven
   probe pod, or both; #2898's acceptance is agnostic, the smoke needs exactly one reproducible
   form.
5. **Scale-rung harness home** — extend `test/soak` (e2e-soak-harness driver) to target a cluster,
   or a sibling under `test/scale`; the contracts (§6.3) are fixed either way.
6. **Dispatch-latency emitter detail** — which `runner.*` event carries queue-wait vs. pod-start
   timestamps (§6.3), to be pinned by the placement-provenance schema work (decision record D5).
