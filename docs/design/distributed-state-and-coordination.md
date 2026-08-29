# Distributed state and coordination

**Status:** approved — Goobernetes v1 design. Encodes the PO decision record in
[goobernetes-decisions.md](goobernetes-decisions.md) (2026-08-22).

**Scope:** the state and coordination layer of Goobernetes mode 3 — who owns instance state, how a
stage running in a disposable pod reaches it, how the run journal is authored live, and what happens
when the daemon pod dies. Scheduling policy, the DSL surface, images, and data-plane transport live in
the sibling v1 documents; this document is the coordinator they all assume.

**Provenance.** Part I is the field-evidence analysis promoted verbatim-in-substance from the spike
branch (`spike/goobernetes`, 2026-08-12). Its six-mechanism inventory and sequencing argument are the
foundation the PO record's D5 ratifies ("designing them separately produces three coordinators").
Part II is the v1 design built on it.

**Supersedes.**
- The **closed-run projection model as journal authority**: `CompletedRunReconciler` projecting only
  CLOSED Temporal runs (`internal/engine/completed_runs.go:27-35`, `cmd/goobers/engineprojection.go:16-51`)
  and close-time backdated span synthesis (`internal/engine/spans.go`, the documented "first-cut
  tradeoff") are superseded by the live journal service (§7). Projection is demoted from authority to
  repair tool, not deleted.
- The **file-drop control seam for distributed runs**: `<SchedulerDir>/pending-triggers/` and the
  `pending-claims` request-file delegation (`cmd/goobers/claims.go`) are superseded in mode 3 by the
  write API (§6). Modes 1/2 keep the file seam through v1 for compatibility.
- The `v2-cloud-scale.md` §"Temporal is the multi-node answer, not distributed flocks" posture is
  **narrowed**: still true for run *orchestration*; no longer the whole answer for journal authorship
  and claims, which get a service seam here.
- Per decision record D5: the tier-3 scheduler fork (`internal/scheduler`) is deleted and
  `cmd/goober-runtime` is retired (#2055 resolved: supersede).

Decision-record rulings are cited as **D0–D12**; this document's own decisions are **DS1–DS10**.

---

# Part I — the field evidence

## 1. Why this document exists now

An OS-spanning run works. On 2026-08-12 run `300534f6f9503e251374d9433060ebf8` executed three stages
across a Linux node and a Windows node in one AKS cluster, on one workflow definition, with one run
id — dispatched by Temporal, with stage artifacts exchanged through a shared content-addressed store.

That was not supposed to be possible yet. The mixed-OS rung was parked behind "claims off files" on
the assumption that distribution needed the claims migration first. It did not, and understanding
*why* is the whole basis of this design:

> **A worker is not an instance.** It owns no claim ledger, mints no runs, serves no API. It polls a
> queue, provisions a workspace, executes one stage, and returns a result envelope.

Everything that is hard about distributing Goobers is hard because it lives on the *instance* side of
that line. Everything that turned out to be easy was already on the worker side. So the useful
question is not "how do we distribute Goobers" but "what is still on the wrong side of that line, and
what is each thing costing."

## 2. The line, drawn precisely

A stage executing somewhere else needs four things. Three of them already work.

| # | What a remote stage needs | Mechanism | State |
|---|---|---|---|
| 1 | To be told to run | Temporal task queue, per **activity** (`ActivityOptions.TaskQueue`) | **works** |
| 2 | A place to work | fresh worktree per attempt, provisioned from the repo, discarded after | **works** |
| 3 | Its predecessor's outputs | `ResultEnvelope` through Temporal history | **works** |
| 4 | Its predecessor's *artifacts* | `cp.Artifact.Resolve(journalRoot)` — a **local filesystem path** | needed the store below |

(4) is worth dwelling on because it is the template for everything left. It was not a missing
feature. It was a *correct* implementation of an assumption — one runner, one directory — that stops
being true the moment there are two. The failure mode was not an error message about distribution; it
was a missing file, reported as an integrity fault, from code that was behaving exactly as designed.

Every remaining item on the list has that shape.

## 3. The pattern: identity without location

The fix for (4) generalises, and it is the single design idea in this document.

`journal.Ref` was already a sha256 of the scrubbed bytes. That means a blob's **identity is already
independent of where it lives** — which is the one property a distributed data plane needs, and it
was sitting there unused. So the node-local run directory becomes a *cache* in front of a store the
fleet shares (`internal/blobstore`): write through on record, fetch any digest not held locally
before the stage runs (`internal/workerhost/materialize.go:15-40`). `Resolve` still reads a local
path. `journal.Run` and the entire local runner are untouched. An instance with no store configured
behaves byte-for-byte as before.

Two properties do the real work, stated as design rules:

**Idempotent writes replace locks.** `Put` of a digest already present is a no-op, published by
rename. Two workers racing on one digest are, by construction, writing identical bytes — so the write
is made *idempotent* instead of *serialized*. That is flock's job, obtained without flock. The
operational consequence is sharp: the store is safe on precisely the storage a journal is not. Azure
Files has no dependable POSIX locking, which is why blessing it as rwx-capable is a false green for
the journal and the claim ledger (#2854) — those coordinate *by locking a shared file*. A
content-addressed store never locks, so the same share is entirely sound for it.

**Names that arrive over the wire are inputs, not paths.** A digest reaches the fetch layer from a
`ContextPointer`, which travels through Temporal history from an upstream stage. An unvalidated one
is a path traversal with a remote origin. Validate before it becomes a path — the same reason
`ValidRunID` exists, applied to digests.

Generalised: **make the thing addressable by identity, make the write idempotent, and the
coordination problem disappears instead of moving.** Where that is not possible, you need a real
coordinator — and the point of this document is that we should know which of those we are choosing,
each time.

## 4. What is still node-local, and what each one costs

Six mechanisms, in the order they will bite.

### M1. Code state lives in the worker's git mirror

Every stage attempt gets a fresh worktree on the run branch and discards it when the stage ends —
good. What survives is the **branch**, in that worker's mirror clone. On one node this is invisible
and free. On another node the mirror is a different clone and the commit is simply absent; the
worktree layer would silently provision a pristine branch off base, which "looks exactly like the PR
legitimately contains nothing" (`internal/worktree/worktree.go:50-72`).

**Cost:** a stage that hands work to another platform must push first.
**Fix:** no new mechanism — a rule the DSL could not state and the compiler could check. A platform
hop is a push boundary (#2861 shipped exactly this for cross-OS).

### M2. The claim ledger is per-node

Each worker mounts its own volume, so `claims.json` exists **twice**, diverging silently. The ledger
is explicitly "designed for one embedded scheduler per instance (SCH-040)"
(`internal/localscheduler/claim.go:58-75`); `flock` on a local file excludes nothing across machines.

**Cost:** claims-mutating stages are single-placement by necessity, and nothing in the DSL says so.
**Fix:** this is the one that genuinely needs a coordinator. A claim is a mutually-exclusive lease
over an external identity (an issue, a PR) — it has no content-addressable identity to exploit, and
two claimants must actually be arbitrated.

### M3. Triggers arrive by file drop

External processes talk to a running daemon by writing a request file into
`<SchedulerDir>/pending-triggers/`, which the daemon polls. Every spike run was fired by
`kubectl exec` **into the same pod**, and worked only for that reason.

**Cost:** no trigger, HITL gesture, or external integration can reach the daemon from anywhere else.
**Fix:** a write API. Four consumers want the same primitive — HITL escalation resolution, remote
executors recording into a journal they do not own, external triggers, and the local seam itself.
Design it *with* the claims work, not after: the claims ledger is plausibly just the first table
behind it.

### M4. Journal authorship is an in-process interface

The recorder is a Go interface holding a `*journal.Run`; `journal.Create` fails closed on an existing
run directory. This is why a worker stages artifacts rather than journaling them, and why the
projection has to author the journal afterward from history. Nuance worth keeping: `journal.Recover`
already exists and is exactly the attach-and-append primitive a second process needs — correct for
two processes *on one node*, inheriting M2's problem across nodes.

### M5. `up.lock` assumes one daemon, and two files depend on that silently

`trigger-evaluations.json` and `schedule-demand.json` have no lock and no generation check
(`cmd/goobers/up.go:333-356` for the singleton). Their safety is entirely inherited from the
single-daemon guarantee, and nothing in either file's code says so.

### M6. Workspace locality is bound to local PID identity

Which breaks between two *pods on one node*, not just across machines — PID namespaces collide
(`internal/worktree/doc.go:13-19`: "distributed workers must use pod-private roots").

## 5. Sequencing

The ordering follows from cost, not from difficulty.

1. **The compiler rule for M1** — cheap, prevents a silent wrong answer (landed as #2861; D6
   generalises it from cross-OS to every cross-runner handoff).
2. **The write API and the claims store, designed together.** M2 and M3 are the same problem wearing
   different clothes, and M4 is the same seam again from the journal's side. Designing them
   separately produces three coordinators. *(This is what Part II funds.)*
3. **M5 and M6 fold into whatever step 2 lands as**, or get explicit guards in the meantime.

Scheduling across nodes and concurrency policy deliberately come *after* — both are policy over a
coordinator, and building them first means building them twice (recorded again in #3278's non-goals).

**The falsifiable claim** stands: if this design is right, distribution keeps arriving as
something that looked structural turning out to be an assumption, fixed by giving the thing an
identity that does not depend on where it is. Two of the spike's three "structural blockers" were
already-existing capabilities nobody had used (`journal.Recover`, per-activity task queues). When a
limitation looks structural, check whether the substrate already has the feature.

Field evidence filed upstream: #2854, #2860, #2861, #2862, #2863, #2864, #2865.

---

# Part II — the v1 design

## 6. Decisions

| # | Decision | Why |
|---|---|---|
| DS1 | **One daemon, one instance root, daemon = control plane.** Claims, provider quota, open-PR caps, fairness, and admission stay in the single daemon pod on RWO storage; no second replica in v1. The daemon is the single writer of `claims.json` and of the scheduler-state files beside it. Ledger-touching stages mutate them only through the claims/state planes (`internal/claimsclient`'s HTTP backend, selected by `GOOBERS_CLAIMS_ENDPOINT` + a claims bearer); the file-locked in-process path remains the daemon's own and the type-1/type-2 same-host path. Item **selection** (which candidates to try, in what order) belongs to the stage; **admission** (whether a lease is granted) belongs to the plane, and acquire's refusal is the only arbiter of contention (decision 005 R2) | D5. #2053's fenced-lease work is the price of a second replica and stays deferred with recorded rationale; "two replicas against one instance root is a refused state" (#2053) remains enforced |
| DS2 | **The daemon write API is the only instance-state path for mode 3.** v1 surface: claim/release for ledger-touching stages, external trigger ingestion, HITL escalation resolution, live journal ingestion, stage-credential issuance | D5. Removes kubectl-exec-only operation; one API, one auth story, one place M2/M3/M4 converge instead of three coordinators |
| DS3 | **API contract first, store second.** Claim/release semantics (lease, epoch, exactly-once settle) are the API contract; `claims.json` may remain the v1 store behind it | The store is swappable behind a contract; the contract is not swappable behind a store. Migrating the file to SQLite/Postgres later is invisible to every caller |
| DS4 | **Live journal service: activities emit journal events as they happen; the daemon's single writer owns sequence allocation; every emit carries an idempotency key** | D0/D5: the journal is the *product output* and must not be an after-image of an implementation detail. Decoupling it from Temporal history keeps Temporal revisitable (D0) and makes runs live — stall detection (`StalledRunTimeout`), SSE, and the portal work mid-run, which D5 makes a v1 functional requirement |
| DS5 | **History projection is demoted to repair.** `ProjectRun` and the reconciler are retained as a backfill/repair path (deterministic re-projection, #629) and as the conformance cross-check, never the primary author | Deleting it would discard the only independent reconstruction of a run; keeping it primary would keep the product output hostage to Temporal history (contradicts D0) |
| DS6 | **Claim-lease renewal is keyed off the ledger plus engine liveness, not in-process run tracking.** The renewal loop enumerates held claims and renews any whose RunID maps to an open Temporal workflow | A daemon restart must not expire a live distributed run's claims. Today `renewLiveClaims` renews only runs "this process is tracking" (`internal/localscheduler/claim.go:459-479`) — state a restart loses while the run keeps executing elsewhere |
| DS7 | **Daemon pod loss is an accepted, bounded v1 outage window.** In-flight stage work continues; instance-state operations stall and retry on the infra budget; failover (fenced leader lease) is deferred | D5/D11. The smoke test proves classification, not continuity. portal-read-architecture §13.3's fenced lease is the recorded follow-on, gated with #2053 |
| DS8 | **Blobstore GC is reachability-keyed**: a digest is evictable only past the adoption watermark AND unreferenced by journal artifact pointers; conservative TTL baseline; run completion is an eligibility signal, never the trigger | D6 verbatim. "Evict on run completion" races span adoption and orphans journal pointers |
| DS9 | **Stage pods hold no instance config and no secret-store access.** The daemon mints stage-scoped, short-lived credentials covering exactly the stage's declared credential capabilities; dispatch payloads carry opaque references only | Honors the #2931 ruling (constrain-and-enforce, references-only, fail-closed canary at dispatch — decision record Goobers-Review/Goobernetes-v1/decisions/0002) and reverses the current worker model of full config + secret stores per node (`cmd/goobers/workerwiring.go:100-160`), which per-stage pods would multiply |
| DS10 | **Credentials are re-resolved at stage start, never snapshotted at dispatch or pod creation, and a stage may re-resolve mid-stage** | The #3489 lesson: the daemon holds a refreshing token *source* but a stage receives a frozen *string*; a 22-minute `ci-poll` started valid and 401'd mid-stage. A snapshot's remaining life is unknowable at mint (`internal/githubapp/source.go` refreshSkew is 5 min); stage duration must never be bounded by it |

## 7. The daemon write API

One versioned surface, extending the existing `/api/v1` contract (`internal/apicontract/contract.go:20-47`
— routes there already carry cost classes and budgets; the write routes join that discipline). Five
planes:

**Claims plane** — `claim`, `renew`, `release`, `settle`. The ledger-touching deterministic stages
(`backlog-query --claim`, `close-out`, `release-claim`) stop reading `GOOBERS_INSTANCE_ROOT` and call
the API. The exactly-once contract (lease + provider-marker epoch settle, `claim.go:42-56`) is the
API's contract; arbitration happens where the ledger lives. The CLI's bolted-on cross-process layer
(`withClaimLock`, `cmd/goobers/providercmd.go:644`; `pending-claims` file delegation) is subsumed on
this path. D9's solver rule — ledger-touching stages never place on Windows — becomes unnecessary as
a *state* restriction (any pod can call the API) but survives v1 as a scheduling default.

**Journal plane** — `emit` (batched events), `adopt-span` (by digest). See §8.

**Trigger plane** — external trigger ingestion. Replaces the `pending-triggers` file drop for
anything outside the daemon pod. The daemon validates, dedupes, and mints the run exactly as the
poll-loop path does today; webhook delivery dedup stays daemon-local (in-memory is safe under DS1).

**HITL plane** — escalation resolution. A human gesture (approve/deny/redirect on an escalated run)
lands as an authenticated API call and is journaled as the resolution event. This is what makes D11's
"triggers + HITL through the write API" smoke item testable.

**Credential plane** — `resolve` for stage pods. See §11.

Exposure rides the existing fail-closed posture: non-loopback bind requires TLS + configured auth
with no insecure override (`internal/instance/config.go:309-318`, #640), and the shipped OIDC
authenticator (`internal/oidcauth`) covers humans (HITL, triggers). Pod-to-daemon authentication is
an open point (§13) — the candidates are projected service-account tokens verified via TokenReview,
or per-run bearer tokens minted at dispatch and carried as opaque references (#2931-clean).

## 8. The live journal service

**Authorship model.** Workers still never touch journal *files* — that invariant survives. What
changes is that they no longer wait for history projection either: each activity emits its journal
events to the daemon's journal plane *as they happen*. The daemon's single writer — the same
per-run single-writer discipline that exists today (`internal/journal/lock.go:17-23`), and the
`InstanceLog.Append` flock-held seq allocation for instance events
(`internal/journal/instance.go:86-140`) — owns **sequence allocation**. Seq is assigned at
acceptance, in arrival order per branch; emitters never propose sequence numbers.

**Idempotency.** Temporal retries re-execute activities. Every emitted event carries an idempotency
key derived from `(runID, branch, stage, attempt, event-ordinal)` — deterministic within an attempt,
distinct across attempts (a repass is a new attempt and journals as one). The writer dedupes on the
key: a retried activity replaying its emissions is a no-op, and the journal contains exactly one copy
with the originally-allocated seq. This is the §3 rule applied to the journal: the event gains an
identity independent of delivery, and the write becomes idempotent instead of coordinated.

**Decoupled from Temporal history (the product-output argument).** D0 states the product is the DSL
and the output — the run journal above all. A journal that exists only as a post-hoc projection of
Temporal history makes the product output a derivative of an implementation detail we explicitly hold
loosely. With live emission, Temporal history remains the engine's *execution* record (retries,
determinism, replay) while the journal is authored by the product's own writer in product terms. If
Temporal is ever swapped (D0 reserves the right), the journal service contract is untouched.

**What runs live buys, concretely.** Stall detection keys off journal liveness
(`RunConditions.StalledRunTimeout`) — under the closed-run projection model a distributed run is
journal-silent until terminal, so a wedged mode-3 stage is *undetectable*. SSE and the portal read
the journal through the daemon (`internal/readservice`; read.db is pinned to the daemon's RWO volume,
so all reads flow through the daemon API regardless). Live **stage-transition visibility is a v1
functional requirement** (D5); per-token streaming is not. Placement provenance (runner name, node,
OS, image, attempt) is journaled under the `runner.*` namespace, non-conformance-normative, surfaced
via new StageAttempt read-model fields (D5).

**Failure policy.** A journal emit that cannot reach the daemon retries on a bounded budget inside
the activity; exhaustion fails the attempt as `attemptClass: infra` (#3361 — infrastructure never
charges the work budget). The event is not droppable: an effect that cannot be journaled did not
happen, which is the same fail-closed stance the projection took (`ErrUnprojectable`).

**Conformance.** The journal invariant is unchanged: equivalent normative event sets, `(branch, seq)`
ordered, across runners (A2 harness). The retained projection (DS5) doubles as the cross-check: a
periodic reconciler diffs the live-authored journal against a history re-projection and files
divergence into the #2871 parity ledger rather than silently repairing it.

## 9. The six mechanisms, resolved in v1

| Mechanism | v1 resolution | Where it lands |
|---|---|---|
| M1 git mirror/branch | Declared handoff edges; compiler rejects undeclared cross-runner chains; runtime pushes the run branch at declared boundaries, next stage fetches (D6, generalising #2861) | Data-plane doc; no coordinator needed |
| M2 claim ledger | Stays daemon-owned single-writer (DS1); *access* moves behind the write API's claims plane (DS2/DS3); lease renewal re-keyed (DS6) | Behind the API |
| M3 triggers | Trigger plane of the write API; file drop retained for modes 1/2 only | Behind the API |
| M4 journal authorship | Live journal service; daemon writer owns seq; idempotency keys (DS4) | Behind the API |
| M5 `up.lock` silent dependents | Stay daemon-local — safe under DS1 by construction. v1 adds the cheap explicit guard the spike doc asked for: a generation/ownership check so a future second daemon trips an error, not a data race | Daemon-local; real fix deferred with #2053 |
| M6 PID-bound workspace liveness | Pod-private worktree roots (the worktree doc's own rule); a fresh pod per attempt makes cross-process PID collision structurally impossible within a run. The pinned-workspace mode's PID lease has no distributed analogue and is unsupported in mode 3 v1 | Dissolved by the pod model |

Provider quota and MaxOpenPRs admission caches stay per-daemon-process in-memory — sound under DS1
because *admission happens before dispatch, in the daemon*; stage pods execute provider effects but
never make admission decisions. #2053 names these fatal for N replicas; that is the deferred
multi-daemon problem, not a v1 one.

## 10. Daemon pod loss and claim leases

**Renewal (DS6).** The renewal loop's source of truth becomes the ledger itself: enumerate
`ClaimEntry` rows, and for each RunID that maps to an open Temporal workflow (describe/query),
`RenewEntry`. A freshly restarted daemon rebuilds its renewal set from ledger + engine before
`RecoverExpired` (`claim.go:553-579`) is permitted to run — that ordering is load-bearing and gets a
test. Result: a daemon restart of any duration shorter than the lease (30 min default,
`DefaultClaimLease`) is invisible to a live run's claims, and even a longer outage cannot reap a
claim while the reaper itself is down; only a restarted daemon that sees a *closed or vanished*
workflow lets the lease lapse.

**Outage window (DS7).** With one daemon pod on RWO storage, pod loss means, until Kubernetes
reschedules onto the volume: no new run admission, no claim/release, no trigger or HITL ingestion, no
journal emission. What does *not* stop: in-flight stage activities keep executing; blobstore
write-through continues (idempotent, daemon-free); Temporal retains all history. Stage attempts that
need the API during the window retry and, on exhaustion, fail as infra attempts — replayed on the
infra budget when the daemon returns, invisible to conformance (#3361). D11's smoke explicitly kills
the daemon pod during each stage class and requires exactly this classification.

**Failover is deferred, deliberately.** The fenced leader lease (portal-read-architecture §13.3) and
the CAS migration of per-process state are #2053's acceptance criteria and remain its scope. v1
records the constraint the deferral creates: the outage window is bounded by pod reschedule time onto
the RWO volume, and any v1 SLO conversation is about that bound, not about eliminating it.

## 11. Credential delivery for stage pods

The current tier-3 worker loads the full instance config directory and secret stores on every node —
per-stage pods would multiply that blast radius by pod count. v1 inverts it (DS9):

- **Pods hold nothing at rest.** No instance config, no secret-store credentials, no long-lived
  tokens in the image or pod spec.
- **References only in dispatch** (#2931 ruling, honored as decided): `env.Inputs` and every Temporal
  payload carry opaque references; the fail-closed canary at dispatch asserts no known credential
  value appears in the serialized envelope. #2953's auth-profile model layers on this same
  enforcement point later.
- **Resolve at stage start (DS10):** the stage pod calls the credential plane, authenticated as
  itself, and receives short-lived credentials minted daemon-side, scoped to exactly the stage's
  declared credential capabilities — capability-gated fail-closed exactly as `buildCredentialEnv` is
  today (`internal/harness/environment.go:31-90`): nothing materializes for an undeclared capability.
- **Snapshots must not outlive stages (#3489):** the mint response carries the credential's TTL, and
  the resolution is performed *at stage start*, never inherited from dispatch time or pod-creation
  time (relevant to D9's warm-pool deferral: a pre-warmed pod holds no identity; what it may hold
  pre-claim is the open point D9 records). For stages that can outlive the TTL (`ci-poll` is
  structurally the most exposed — it polls precisely because CI is slow), the injected value is not
  final: the stage re-resolves through the same plane on an auth failure (one re-resolve-and-retry on
  401) and stage `timeoutSeconds` ceases to be silently bounded by token life.

## 12. Relationship to the blobstore and GC (D6)

The blobstore stays the identity-not-location plane of §3, unchanged in contract: write-through
before pod disposal, materialize-before-stage, `Put` idempotent-by-digest, never overwrite. Two
couplings to this document:

- **Adoption moves earlier.** Under live journaling, artifact pointers and span adoptions are
  journal-recorded during the run, not minutes later by a closed-run reconciler — the "projection
  watermark" that GC previously had to respect largely collapses into "what the journal already
  references." The repair projection (DS5) still adopts by digest, so the watermark concept survives
  for backfill.
- **Reachability-keyed eviction (DS8):** a digest is evictable only when it is past the adoption
  watermark AND unreferenced by any journal ArtifactPointer. v1 ships a conservative TTL baseline on
  top; run completion is an eligibility *signal* feeding the policy, never the trigger by itself —
  eviction between a gate verdict and its repass must be impossible while the run's journal still
  references the digests involved.

## 13. Acceptance criteria

Falsifiable, aligned with the D11 smoke:

1. A run is triggered from outside the daemon pod through the trigger plane — no `kubectl exec`
   anywhere in the smoke — and completes.
2. A ledger-touching stage executes in a non-daemon pod via the claims plane; two concurrent
   claimants for one backlog item across different pods: exactly one wins, both outcomes journaled,
   zero double-work.
3. The portal (via SSE) shows a stage transition while the run is mid-flight; a deliberately wedged
   distributed stage trips `StalledRunTimeout` before the run is terminal.
4. A forcibly-retried activity re-emits its events; the journal contains exactly one copy per
   idempotency key, and the A2 conformance diff against a local run of the same workflow is empty.
5. Daemon restart mid-run: no claim held by the live run expires during the window; renewal resumes
   from ledger + engine liveness; `RecoverExpired` provably does not run before the renewal set is
   rebuilt; the run completes.
6. Daemon pod kill during each stage class (D11): affected attempts classify as
   `attemptClass: infra`, never charge the work budget, and the run completes after the daemon
   returns.
7. No resolved credential value appears in any Temporal payload (dispatch canary, #2931); a stage
   whose declared capabilities are empty can resolve nothing.
8. A stage running longer than its credential's TTL completes via the re-resolve path — the #3489
   shape, reproduced and passed.
9. GC never evicts a digest referenced by a journal ArtifactPointer or pending adoption; a repass
   after a long park still materializes its verdict pointer and prior artifacts.

## 14. Open implementation points

Not re-opened decisions — implementation questions the design deliberately leaves to the epics:

- **Pod-to-daemon auth mechanism** for the write API: projected service-account tokens + TokenReview
  vs per-run minted bearer references. (Human auth is decided: the shipped OIDC seam.)
- **Claims store backend** behind the API contract (retain `claims.json` vs a SQLite table on the
  daemon volume) — DS3 makes this swappable; pick at implementation with a migration note.
- **Emit batching, retry budget, and buffer bounds** for the journal plane before an attempt
  infra-fails; whether a pod-side spool-and-forward is worth its complexity in v1.
- **Idempotency-key retention window** at the writer (how long dedup state is held after run close).
- **Renewal probe mechanics**: Temporal describe-per-claim vs visibility queries, and the poll rate
  at which renewal load stays negligible.
- **Outage-window bound**: the v1 number (pod reschedule + volume reattach on the reference
  substrate) is measured during the smoke, not promised in advance.
- **Mid-stage credential refresh cadence** vs on-401-only re-resolve, pending #3489's confirmation
  evidence (mint-timestamp check; second occurrence in a long-poll stage).
- **File-seam deprecation schedule** for modes 1/2 (`pending-triggers`, `pending-claims`) once the
  loopback API path is proven equivalent.
- **`runner.*` provenance event schema** details (journal event schema changes are versioned) and the
  StageAttempt read-model fields that surface them.

## 15. Issue cross-references

| Issue | Disposition here |
|---|---|
| #2053 | Deferred with rationale (DS1/DS7): fenced lease + CAS migration remain its scope, gated before any second daemon replica; M5 guard lands now |
| #2051 | Narrowed: the journal stays on the daemon's POSIX volume with a service seam in front; the blob-portable journal rewrite is not required for v1 |
| #2055 | Resolved: supersede — tier-3 scheduler fork deleted, `cmd/goober-runtime` retired (D5) |
| #2854 | Encoded in §3: RWX false-green stays refused for journal/ledger; sound for blobstore |
| #2860 / #2861 | Implemented / generalised per D4 and D6 (M1) |
| #2866 / #2907 | The artifact plane this design builds on; adoption timing updated by §12 |
| #2871 / #3361 | Parity ledger receives live-vs-projected divergences; infra attempt classing governs API-unreachable failures |
| #2931 / #2953 | Ruling honored (DS9); auth profiles layer on the same enforcement point later |
| #3482 | Increment 1 (D1): moving execution out of the API pod is the first consumer of this seam |
| #3489 | Root lesson encoded as DS10; confirmation evidence is an open point |
| #640 / #2901 / #644 | Exposure posture and portal auth ride D10; the write API inherits the fail-closed bind rule |
