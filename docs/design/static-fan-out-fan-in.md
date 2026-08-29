# Design: Static fan-out/fan-in — bounded parallel branches and a real join

> Status: **implemented — GA in #1939** · Area prefix: `FO` · Milestone:
> **Versioning & Releases — DSL compatibility + tagged builds** (#12)
> Resolves the **static half** of #1310 — the explicit parallel failure policy and the
> bounded `parallel` construct, spec'd against the conformance surface. #1310's
> `for_each` (data-driven width) is an explicit non-goal here (§11) and stays open.
> Partially supersedes #155's local-runner premise (§4).
> Requirements: [`config-as-code.md`](../requirements/config-as-code.md) (`CFG-022`),
> [`gaggle.md`](../requirements/gaggle.md) (`GAG-010`),
> [`workflow.md`](../requirements/workflow.md) (`WF-002`, `WF-015`, `WF-020`, `WF-051`)
> Architecture: [`ARCHITECTURE.md`](../ARCHITECTURE.md) §3.2, §3.3, §4
> Companion: [`dsl-version-lifecycle.md`](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/design/dsl-version-lifecycle.md) (`DVL` — the
> version treatment in §8; note that doc still reads "not implemented", but the
> machinery has since shipped — see §3 row 8)
> References: `internal/workflow/internal/model/machine.go`,
> `internal/workflow/compile.go`, `internal/journal/state.go`,
> `internal/journal/event.go`, `api/v1alpha1/workflow_types.go`,
> `internal/supportmatrix/supportmatrix.go`
> Related issues: #1310, #155, #562, #817, #1427, #1430

## 1. Decision

The DSL gains **one new state kind** — a `parallel` state — that fans a run into a
statically-declared, named set of branches and joins them at a single successor
state once every branch has settled. Branch width is fixed at author time. The
construct is **core DSL, implemented by the local runner, inside the cross-runner
conformance surface** — not a tier-3 extension (§4).

```text
churn-analysis ──▶ ┌─ security ──────┐
                   ├─ performance ───┤──▶ collate ──▶ nominate
                   └─ test-coverage ─┘
```

Three things make this a real join rather than a convention:

1. **The join runs exactly once**, after every branch reaches a branch-terminal, and
   it can read every branch's outputs and artifacts (§7).
2. **The failure policy is declared, never implied** — `fail_fast`,
   `all_or_nothing`, or `continue_on_error`, with defined journal and routing
   semantics for each (§5.3). This is the lesson #1310 carries forward: parallelism
   without an explicit failure policy bakes ambiguity in permanently.
3. **A branch that never settles is a defined outcome, not a hang** — every parallel
   declares a per-branch timeout, and the join receives an explicit completeness
   record (§5.4).

What is *not* in scope: dynamic (data-driven) branch width, nested parallels, child
workflows, and cross-branch communication (§11).

## 2. Driving use case — quality-sprint, and why the workaround is not acceptable

The immediate consumer is a **quality-sprint** producer workflow: on a schedule, one
analysis stage builds a churn/risk report over recent history; N read-only research
agents each review the repo through one scoped lens (security, performance,
maintainability, test coverage, dependencies, latent bugs); a lead agent then reads
every report, deduplicates findings across lenses, applies one severity taxonomy, and
emits a unified artifact that `work-nomination` turns into backlog items.

That shape is *expressible today* only by exploding it into N+2 separate workflow
definitions: one producing the churn report, N focus-area workflows each triggered
off it, and a lead workflow whose first stage idle-polls until all N reports exist.
It is a functional hack, and it is the wrong thing to ship as the answer:

- **The shared upstream is duplicated per branch.** The churn analysis is either
  recomputed N times or hand-threaded through an artifact each focus workflow must
  re-resolve. Every future shared stage duplicates again.
- **The cron is duplicated N times** and the N schedules drift relative to each
  other, so the polling join either stalls on a straggler forever or starts the lead
  agent against a partial set. Neither is detectable from the journal.
- **There is no run.** N+2 runs means no single journal, no single trace, no single
  budget, no single verdict — the collation is real work with no provenance tying it
  to the evidence it collated.
- **Adding or removing a lens is an N-file edit**, and nothing validates that the
  lead workflow's expectations still match the set that actually exists.

The engine debt is the point. Quality-sprint is the driving requirement, not the
deliverable — this document specifies the primitive, and quality-sprint is filed
separately against it (§10).

## 3. What exists today (verified against `origin/main`)

| # | Component | State today | Anchor |
|---|---|---|---|
| 1 | Task successor | `Next string` — exactly one successor, empty means terminal. | `api/v1alpha1/workflow_types.go:232` |
| 2 | Gate successors | `Branches map[outcome]target` — many *declared* edges, exactly one *taken*. Branching, not forking. | `api/v1alpha1/workflow_types.go:334` |
| 3 | Machine transitions | `Outgoing()` already returns `[]string`, so its *signature* survives — but `Machine` holds only `tasks` and `gates` maps, and `Has()`/`Outgoing()` return nothing for anything else. A third state kind touches `Has`, `Outgoing`, `stateNames`, `reachabilityProblems`, `precedingTasks`, the graph projection, and `api/validate/validate.go:888,910`. | `internal/workflow/internal/model/machine.go:48-54,91-117` |
| 4 | Reserved targets | `@abort`, `@escalate`, plus `""` for complete. `IsReservedTarget` means specifically a reserved **terminal** action, and `isTerminal()`/`canExit` and `validate.go:888,910` all rely on that — so it is *not* the admission point for a non-terminal `@join` (§5.2). | `internal/workflow/internal/model/machine.go:30-34`; `v_current/types.go:29-31` |
| 5 | Run checkpoint | `State.MachineState string` — **one** cursor. Explicitly *derived* and **EXCLUDED from conformance**; always reconstructable from the event journal. | `internal/journal/state.go:28-52` |
| 6 | Journal envelope | `Branch int` **already exists and is already normative** as the secondary ordering key. Its doc comment says "0 at tiers 1–2; reserved for tier-3 parallel branches." | `internal/journal/event.go:147-149` |
| 7 | Conformance relation | Already specifies that parallel-branch events order by `(branch, seq)` — but scopes that to tier 3. | `docs/ARCHITECTURE.md:88-89` |
| 8 | DSL versioning | Shipped. Per-workflow `dslVersion` pin, host support matrix, and **multiple coexisting interpreters** routed by version. | `internal/workflow/compile.go:184-208`; `internal/supportmatrix/supportmatrix.go:42-47` |
| 9 | Cross-stage data flow | `inputsFrom` reads **only the immediately preceding stage** — the validator's own diagnostic hard-codes that. But `precedingTasks()` already computes a predecessor *set* by inverting the edge list. #562 (approved) extends it to stage-qualified references. | `v_current/stagecontract.go:177-179,191-217`; #562 |
| 10 | Stage outputs | `Outputs` are **scalars only** (`string`/`number`/`integer`/`boolean`/`null`). Anything bulk must already be an artifact. | `api/schemas/result.schema.json:11-17` |
| 11 | Artifact handoff | Context pointers are **positional** — `<stage>.artifact[i]` — and accumulate monotonically into a walk-local slice handed to every later stage. | `internal/runner/run.go:3186` |
| 12 | The walk | A single-cursor `for` loop over one `state` variable. No goroutines, no active set. `pointers`, `lastStage`, `lastResult`, `workspaceBranch` are loop-local. | `internal/runner/run.go:850-1114` |
| 13 | Stage workspaces | Each stage gets its own git worktree, but **all on one run branch** (`BranchNameIn(ns, workflow, runID)`). Git forbids two worktrees on one branch. Scratch (non-repo) workspaces have no such constraint. | `internal/runner/run.go:3005-3061` |
| 14 | Resume | Every reconstruction helper (`reconstructPointers`, `lastFinishedSubject`, `lastWorkspaceBranch`, `gateRepassSeed`, …) is a backward "last X wins" scan assuming total ordering. | `internal/runner/resume.go:361-382,731-902` |
| 15 | Feature registry | Every DSL surface element carries a `FeatureID` with a `preview → ga → deprecated → removed` lifecycle. Preview features are gated behind the `goobers.dev/allow-preview-features` instance annotation. | `v_current/features.go:246-257,622-625`; `v_current/compile.go:56` |

The verdict from that inventory: **the contracts were built for this and the runtime
was not.** The journal envelope, the conformance relation, the transition signature,
and the reserved-target admission point all already accommodate branches. What is
genuinely missing is (a) the DSL surface, (b) a multi-cursor run state, (c) the runner
loop that advances more than one cursor, and (d) a per-branch workspace story (§6.5) —
which is the one constraint that is not merely "generalise a scalar to a set."

## 4. The architectural call: static parallel is core DSL, not a tier-3 extension

`docs/ARCHITECTURE.md:74-77` currently says parallel branches are **tier-3 DSL
extensions**
— compile-admitted only for tier-3-targeted definitions, outside the cross-runner
conformance surface, deferred "until the local runner implements them." `CFG-022` and
`GAG-010` carry the same carve-out as their one declared exception to tier-portability.

**That carve-out is now wrong, and this design reverses it for the static case.**

- The local runner is the runner that **ships and is used**. Tier 3 is V2. Leaving
  parallelism on the tier-3 side of the seam means the capability does not exist for
  any real user for the foreseeable future — while the demand (quality-sprint, and
  every monorepo-scale workflow after it) is present now.
- The carve-out is the only declared exception to `GAG-010` tier-portability and to
  `CFG-022` schema identity, and it shrinks to child workflows alone. It does not
  vanish — `CFG-022` names both, and `GAG-010` says "such as parallel branches," an
  open-ended example that must be re-worded to name child workflows specifically.
  Shrinking a hole to one named feature is a strictly better contract than preserving
  an open-ended one for a feature we are about to implement below the line anyway.
- Ordering branch events by `(branch, seq)` is **already** the specified conformance
  relation (`docs/ARCHITECTURE.md:88-89`). Promoting it from "at tier 3" to "at every
  tier" is a one-clause amendment, not a new mechanism.
- Building it local-first is what *makes* the Temporal mapping honest: the semantics
  get pinned by a conformance corpus that runs on the runner we can actually
  execute today, so the tier-3 implementation has a spec to conform to rather than
  defining the semantics by itself.

What stays deferred to tier 3 / later: **child workflows** and **dynamic branch
width** (§11). #155's scope splits accordingly — its parallel-branches half is
answered here and moves to this milestone; its child-workflows half stays V2.

The amendments this implies (landing as slice **FO-1**, not in this document's PR):

- `ARCHITECTURE.md` §3.2 — strike parallel branches from the tier-3 extension
  sentence, leaving child workflows; §3.3 — change "at tier 3, parallel-branch events
  order by `(branch, seq)`" to state that ordering unconditionally.
- `ARCHITECTURE.md` §3.3's **conformance-set enumeration** (`:85-89`) — add the four
  new event types (§6.2) and the completeness record. Without this they are excluded
  by default and §9's fixtures would assert nothing. `journal.ConformanceView` must be
  extended in the same slice.
- `ARCHITECTURE.md` §3.3's **exclusion list** — `seq` becomes non-normative *across*
  branches (§6.2). This is the one amendment that weakens an existing guarantee, and
  it is load-bearing: two conformant runners that interleave branches differently
  necessarily assign different `seq` values to the same branch events.
- `CFG-022` — narrow the tier-3 schema-extension sentence to child workflows only.
- `GAG-010` — replace "such as parallel branches" with a child-workflows-specific
  wording, so the exception is closed-ended.
- `internal/journal/event.go:147-149` — `Branch` is normative at every tier; 0 means
  the run's root branch, not "tiers 1–2."

## 5. DSL surface

### 5.1 The `parallel` state

A third top-level state collection alongside `tasks` and `gates`. It is a state like
any other: it is named, it is a valid `next`/`branches` target, and it appears in the
graph projection.

```yaml
spec:
  start: churn-analysis
  tasks:
    - name: churn-analysis
      type: deterministic
      run: { command: ["goobers", "churn-report", "--window", "336h"] }
      expectedOutputs: [churn-report]
      next: focus-areas          # ← a parallel state, referenced like any other

    - name: review-security
      type: agentic
      goober: quality-researcher
      goal: Review the repository through a security lens.
      next: "@join"              # ← branch-terminal
    # …review-performance, review-test-coverage, each ending `next: "@join"`

    - name: collate
      type: agentic
      goober: quality-lead
      goal: Deduplicate and triage every branch report into one unified finding set.
      next: nominate           # elided below, as are the other two lens stages

  parallels:
    - name: focus-areas
      failurePolicy: continue_on_error   # so one dead lens cannot discard the rest
      branchTimeoutSeconds: 2700
      maxConcurrentBranches: 4
      join: collate
      # no `onFailure` — it is required for fail_fast/all_or_nothing and
      # forbidden for continue_on_error, where the join owns the decision (§5.3).
      branches:
        - { name: security,      start: review-security }
        - { name: performance,   start: review-performance }
        - { name: testCoverage,  start: review-test-coverage }
```

Branch bodies are ordinary tasks and gates declared in the same `tasks:`/`gates:`
lists. A branch is not a nested scope — it is a named entry point into a subgraph the
compiler proves is disjoint (§5.5).

### 5.2 `@join` — a new reserved target

A branch ends when it transitions to the reserved target `@join`. Using the reserved-
target *vocabulary* rather than an implicit "a branch ends at a task with no `next`"
keeps branch termination **explicit and greppable**, and keeps "no `next`" meaning what
it already means everywhere else (the run completes).

But `@join` must **not** be admitted through `IsReservedTarget`. That predicate means
specifically a reserved *terminal* action (`machine.go:30-34`), and its consumers rely
on that: `isTerminal()` feeds `canExit` (`v_current/types.go:29-31`,
`v_current/compile.go:288-336`), and `api/validate/validate.go:888,910` *skips*
dangling-reference checking for reserved targets. `@join` is reserved but
**non-terminal** — it continues to the join stage. Admitting it there would let
`canExit` treat `@join` as an exit on the root path and let `validate` silently accept
`next: "@join"` anywhere, defeating §5.5 rule 4. It therefore needs its own predicate
(`IsReservedBranchTarget`), and every existing `IsReservedTarget` call site must be
audited to decide which of the two it wants.

`@join` is legal only on a state reachable from a branch `start`. `@abort` /
`@escalate` inside a branch terminate the **whole run** regardless of failure policy:
sibling branches are cancelled at their next stage boundary and recorded `cancelled`,
the join does not run, and the run reaches the reserved target's terminal phase. These
are deliberately the *loud* exits — a branch that wants to fail *softly* returns a
failed status and lets the parallel's failure policy decide (§5.3).

Cycles inside a branch remain legal, exactly as they are today: a gate may route back
to an earlier state in its own branch (the repass pattern), and the existing
`canExit` reachability check (`v_current/compile.go:288-336`) already rejects a cycle
with no exit — extended here to require that the exit be `@join`, `@abort`, or
`@escalate` (§5.5 rule 2). Repasses consume `branchTimeoutSeconds` like any other work.

### 5.3 Failure policy — declared, never implied

`failurePolicy` is **required**; there is no default. The three values are #1310's
vocabulary:

| Policy | On the first branch failure | Remaining branches | Join runs *on failure*? | Route on failure |
|---|---|---|---|---|
| `fail_fast` | Not-yet-started branches are abandoned; started siblings cancel at their next stage boundary | Recorded `cancelled` | **No** | `onFailure` |
| `all_or_nothing` | Nothing — every branch is allowed to finish | Run to completion | **No** | `onFailure` |
| `continue_on_error` | Nothing | Run to completion | **Yes** | join's own `next` |

When no branch fails, all three policies behave identically: the join runs and the run
continues to the join's own `next`.

- A branch **fails** when any stage in it fails terminally after its retry policy is
  exhausted, or when it exceeds `branchTimeoutSeconds` (§5.4). A stage marked
  `continueOnError: true` keeps its existing meaning and does not fail the branch.
- **`fail_fast` and `all_or_nothing` differ only when branches actually overlap.** At
  the default `maxConcurrentBranches: 1` branches run sequentially, so "cancel the
  siblings" means "never start the remaining ones" and `fail_fast` simply skips the
  rest while `all_or_nothing` still runs them. That divergence is real, intended, and
  the reason both policies exist — but it means the *observable* difference between
  them is a function of a concurrency knob, which authors must understand. The §9
  same-projection fixture therefore compares `maxConcurrentBranches: 1` against `> 1`
  only under `continue_on_error`, where no cancellation occurs.
- `onFailure` is **required** when the policy is `fail_fast` or `all_or_nothing` — a
  reserved target (`@abort`/`@escalate`) or a state name, so failure is always a
  *defined branch*, never a silent stop (mirroring `GT-002`) — and **forbidden** under
  `continue_on_error`, where by construction the join always runs and owns the
  decision. Declaring both would state two contradictory owners of the same failure.
- **The join is an ordinary stage.** If the join itself fails, retries, or routes to a
  reserved target, that is existing single-cursor behaviour — the parallel is already
  over by then. The parallel's failure policy governs the *branches*, never the join.
- Under `continue_on_error` the join is responsible for the failure — it receives the
  per-branch completeness record (§5.4) and decides. This is the policy
  quality-sprint uses: one dead lens must not discard nine good reports.
- **Cancellation is cooperative and bounded**: `fail_fast` cancels at the next stage
  boundary, never mid-stage. An in-flight agentic stage runs to its own timeout.
  `fail_fast` is a promise about *not starting more work*, not about instant stop.
- **`no-work` is rescoped to the branch.** Today a `no-work` stage result
  short-circuits the whole run to `PhaseCompleted`, ignoring `Next`
  (`internal/runner/run.go:1692-1704`). Inside a branch that would let one lens
  silently complete the entire quality sprint. Within a branch, `no-work` terminates
  **that branch** with status `succeeded` and an empty output set; only on the root
  branch does it keep its current whole-run meaning. This is a genuine semantic
  addition and needs its own conformance fixture (§9).
- **`blocked` stays a whole-run exit.** `ResultBlocked` terminates the run as
  `PhaseEscalated` unconditionally (`internal/runner/run.go:1573-1580`) — structurally
  the same short-circuit as `no-work`, but rescoping it would be wrong: `blocked` means
  an agent could not proceed and wants a human, which is exactly the `@escalate`-class
  loud exit of §5.2. A branch returning `blocked` escalates the whole run and cancels
  its siblings. Called out explicitly because the two statuses look alike in
  `taskOutcome` and are treated oppositely here.

### 5.4 Branch timeout and the completeness record

Every parallel declares `branchTimeoutSeconds`, bounding one branch. A branch exceeding
it is terminated at its next stage boundary and recorded `timed-out`, then handled
exactly as a failure under the declared policy.

There is no unbounded wait, and the ceiling is statically knowable — but it is a
function of the concurrency knob, not of one branch:

```text
ceil(N / maxConcurrentBranches) × branchTimeoutSeconds
  + max over branch stages of (timeoutSeconds × retry.maxAttempts
                               + retry.backoffSeconds × (retry.maxAttempts - 1))
```

The overshoot term is the last stage's own attempt budget
(`api/v1alpha1/workflow_types.go:236-250`), because cancellation is only checked at
stage boundaries (§5.3). At the default `maxConcurrentBranches: 1` that first term is
`N × branchTimeoutSeconds` — a ten-lens quality sprint at 45 min/lens is a 7.5-hour
ceiling, which authors must size deliberately.

The join stage's invocation carries a **branch completeness record** — for each
declared branch, its terminal status, its branch id, a count of artifacts it recorded,
and pointers to its outputs and artifacts. The statuses are `succeeded`, `failed`,
`timed-out`, `cancelled`, and `no-output` — the last distinguishing "settled without
producing anything" (a rescoped `no-work`, or a branch whose only substantive stage
carried `continueOnError: true`) from a real success, which the other four cannot
express. Under `continue_on_error` this record is the join's primary input, and it is
what makes "did I actually get all ten reports?" answerable from inside the workflow
instead of inferred from a directory listing.

### 5.5 Compile-time validation

All of these fail `goobers validate` — a fan-out mistake must never be discovered at
2am in a live run (`CFG-023`, fail closed):

1. **Disjoint branches.** The subgraph reachable from each branch `start` is disjoint
   from every other branch's, from the join's, from the pre-parallel root path, and
   from any state named by `onFailure`. No edge crosses a branch boundary. This is what
   makes branch attribution total, and it is what #1427's "parallel branches do not
   produce cross-branch phantom edges" needs to be structurally true rather than
   defensively handled. *Consequence authors will hit:* a gate's `escalate` control
   branch (`model.BranchEscalate`) names a real workflow state, so two branches whose
   gates both escalate cannot share one escalation state — each needs its own. The
   diagnostic must say so, since the natural authoring instinct is to share it.
2. **Branch exits are branch-terminal.** Every state reachable from a branch `start`
   can reach `@join`, `@abort`, or `@escalate`, and no branch state may reach `""`
   (complete) or the join state directly. Note this is the *decidable* form: like the
   existing `canExit` fixed point (`v_current/compile.go:288-336`) it is reachability,
   not termination — a back-edge cycle inside a branch that never actually exits still
   satisfies it, and is bounded at runtime by `branchTimeoutSeconds` (§5.2). Promising
   more than reachability here would be promising to solve the halting problem.
3. **The join is parallel-entered only.** No declared edge targets the join state
   except the parallel's own `join:`.
4. **`@join` is branch-scoped.** `next: "@join"` outside any branch is an error.
5. **Bounds.** At least 2 branches, at most a host-declared cap (proposed: 32; a
   `WF-015`-class runaway bound); unique branch names; `maxConcurrentBranches` ≥ 1.
6. **Policy completeness.** `failurePolicy` present; `onFailure` present iff the
   policy is `fail_fast` or `all_or_nothing`.
7. **No nesting.** A branch subgraph may not contain a parallel state (§11).
8. **Timeout coherence.** Each branch's stage timeouts must fit within
   `branchTimeoutSeconds`, so a single stage cannot silently guarantee its branch times
   out. This is a **new check**, not an extension of `CheckStageTimeoutCoherence`
   (`v_current/timeoutcoherence.go:11-60`), which compares a bounded-wait *poll
   interval* against its own stage's timeout for `shell`/`ci-poll` stages and has no
   notion of a budget above the stage. There is also **no workflow-level timeout field**
   in `WorkflowSpec` today, so the §5.4 ceiling is reported by `goobers validate` as
   information rather than checked against a declared budget; adding such a field is
   out of scope here.
9. **No writable repo workspace inside a branch** (§6.5). A branch stage may use
   `scratch` or the new `repo-readonly` mode; requesting a writable run-branch worktree
   fails compile with a message naming the parallel and the stage. Because agentic
   stages currently *hardcode* the writable mode, this rule is only satisfiable once
   FO-4 adds `repo-readonly` and lifts `Workspace` onto the agentic path — before that,
   the rule is enforced instead by pinning `maxConcurrentBranches: 1`.
10. **No human gate inside a branch.** A human gate pauses the run and returns from the
    walk with `PhaseRunning` (`internal/runner/run.go:1023-1029`), and resume restores
    it by unconditionally overriding the single start cursor
    (`internal/runner/resume.go:378-382`) — single-cursor by construction. "One branch
    waits on a human for three days while its siblings hold workspaces" is a new
    suspension model, not a generalisation of the existing one. Human gates before the
    parallel or at/after the join are unaffected. Revisit once §6 is proven.

## 6. Runtime semantics

### 6.1 Multi-cursor run state

`State.MachineState string` becomes insufficient. It gains a sibling: a set of
**branch cursors**, each `{branchId, branchName, machineState, phase}`. The root
cursor is branch id 0. `MachineState` retains its meaning while the run is
single-cursor — the field is not repurposed, so every existing reader keeps working
and the change is additive.

`State` is explicitly *derived* and *conformance-excluded*
(`internal/journal/state.go:23-27`), so this is not a contract change — it is a
checkpoint-shape change, with the event journal remaining the source of truth.

### 6.2 Journal semantics

- Every event emitted while executing a branch carries that branch's `Branch` id.
  **Branch 0 is reserved for the root**, so declared branches are numbered from 1 in
  declaration order — deterministic and reproducible across runs and runners, and
  never ambiguous against a root-branch event. (`Branch` is already `0` on every event
  ever written, so root-as-0 is also the back-compatible reading of every existing
  journal.)
- The parallel state emits `parallel.started` (with the declared branch set),
  `branch.started` / `branch.finished` per branch (terminal status included), and
  `parallel.finished` (with the completeness record and the routing decision) on the
  root branch.
- Conformance compares in `(branch, seq)` order, per the amended §3.3. Within a
  branch, ordering is total; across branches it is not — and it must not be, because
  interleaving is a scheduling artefact, not semantics.
- **The completeness record is the normative artefact of a parallel**, not the
  interleaving. Two runners that schedule branches in different orders are still
  conformant iff every branch's own event sequence matches and the record matches.
- **Appends stay serialized.** The per-run file lock is acquired for the `*Run`
  lifetime (`internal/journal/run.go:30,40-43`) and in-process appends already
  serialize behind `Run.mu` (`internal/journal/run.go:28`). Branches reuse that mutex
  rather than taking per-branch logs — one `events.jsonl` remains the single ordered
  truth.
- **`seq` becomes non-normative across branches.** `seq` is assigned at append time, so
  once branches interleave, two runners that schedule them differently assign different
  `seq` values to the same branch events. `Seq` is currently marked normative
  (`internal/journal/event.go:142-144`); the conformance relation must compare `seq`
  **within** a branch (where it remains totally ordered and meaningful) and ignore its
  absolute value **across** branches. This is the same class of scheduling
  nondeterminism §7 handles for artifact-pointer ordering, and it needs the explicit
  `ARCHITECTURE.md` §3.3 amendment listed in §4 — otherwise §9's fixtures would
  spuriously fail on any runner that interleaves.

### 6.3 Concurrency bounds

Branches execute inside **one** run, so a wide parallel would otherwise be invisible to
the instance's concurrency accounting and could starve every other workflow. Two bounds
apply:

1. **`maxConcurrentBranches`** per parallel — author-declared, default 1, i.e.
   deterministic sequential execution unless the author opts into concurrency.
2. **A new instance-level `maxParallelBranches`** bound. `maxParallelRuns`
   (`internal/localscheduler/conditions.go:109-117`) is documented and understood as a
   cap on *runs*; silently redefining its unit would change admission for every
   existing instance the moment one parallel workflow appears. A separate bound leaves
   the shipped knob's meaning intact.

Claiming is unaffected: a run holds one claim regardless of width, and branch fan-out
never multiplies claims (`WF-031`).

### 6.4 Crash recovery

Recovery replays the event journal and rebuilds every branch cursor from the
`(branch, seq)` stream — the existing `journal.Recover` path
(`internal/journal/reader.go:179-184`), generalised from one cursor to a set. A branch
whose last event is `branch.started` resumes at its recorded stage exactly as a
single-cursor run resumes today. A run that crashed with all branches finished but
before `parallel.finished` re-evaluates the policy from the recorded branch terminals,
which is deterministic — so the join is never double-entered and never lost.

The load-bearing detail: **every resume reconstruction helper is a backward
"last X wins" scan** over a totally-ordered log — `reconstructPointers`,
`lastFinishedSubject`, `lastWorkspaceBranch`, `gateRepassSeed`, `gateDiffSeed`,
`interruptedAttempt` (`internal/runner/resume.go:731-917`) — and the start-cursor
selection that consumes them is a single scalar (`:361-382`). Each must be scoped to a
branch id, and the scalar must become a cursor set. That mechanical scoping pass is the
bulk of FO-3's real cost, and it is why FO-3 lands with recovery tests *before* any
branch actually executes.

`taskOutcome` (`internal/runner/run.go:1569-1726`) is deliberately factored out so
`Resume` applies the identical transition without re-dispatching. Branch-aware
routing must therefore land in **both** call sites, or a resumed run and a live run
diverge — precisely the class of bug the conformance corpus exists to catch.

### 6.5 Branch workspaces — the one genuinely new constraint

Every stage already gets its own git worktree, but they are all created **on one run
branch**, `BranchNameIn(namespace, workflow, runID)`
(`internal/runner/run.go:3005-3061`). Git refuses to check out one branch in two
worktrees simultaneously, so two concurrently-executing repo-backed stages would
collide outright. This is the only part of the design that is not a generalisation of
an existing scalar, and it is the reason `maxConcurrentBranches` defaults to 1
(**OQ-2**).

Today `WorkspaceMode` has exactly two values, `repo` and `scratch`
(`api/v1alpha1/workflow_types.go:289-297`), `Workspace` is a field on
`DeterministicRun` only, and **agentic tasks and gates hardcode `WorkspaceRepo`**
(`internal/runner/run.go:2294-2296`, `:2697`). So there is no read-only workspace kind
a branch stage could request today — and since the driving use case is a fan-out of
*agentic* research stages, every branch in §5.1 would currently demand a writable
run-branch worktree. **The read-only mode is new DSL surface this design must add**, in
FO-4, not a capability to be selected from what exists.

The resolution has three tiers:

1. **Scratch workspaces** (`os.MkdirTemp`, non-repo) parallelise freely today. No
   change.
2. **Read-only repo workspaces — new.** A third `WorkspaceMode` value (`repo-readonly`)
   checking the run's pinned base revision out in **detached HEAD**, plus lifting
   `Workspace` onto the agentic path so an agentic task and an agentic gate can request
   it. No branch name, so no collision and no merge at the join. This is the mode
   quality-sprint's lenses use, and FO-4 must ship it for FO-5 to be usable at
   `maxConcurrentBranches > 1`. It is *not* the same thing as
   `provisionAdditionalCheckouts` (`internal/runner/run.go:3063-3065`), which
   materialises the gaggle's separate reference repos (`AdditionalRepos`, MGV-11
   #1286), not a read-only view of the workflow's own target repo.
3. **Writable repo workspaces** — each branch would need its own branch name
   (`<runBranch>/<branchName>`) *and* a defined merge strategy at the join. There is
   no non-arbitrary merge semantics for two agents that both edited the tree, so
   **writable repo workspaces inside a parallel are rejected at compile time** in this
   design (§5.5 rule 9). Concurrent code-writing branches are a separate problem with
   a separate design; nothing we need today requires it, and admitting it now would
   bake in a merge policy we have no evidence for.

Until FO-4 lands, a parallel is admissible only with `maxConcurrentBranches: 1`, where
branches execute sequentially and never hold two worktrees at once — which is why that
is the default (**OQ-2**) and why FO-5 can ship ahead of wide concurrency.

This makes the primitive's first form precisely "fan out read-only work, fan in its
findings" — which is exactly the shape every driving use case has.

## 7. Fan-in data flow — the hard dependency

The join must read outputs from N branches. `inputsFrom` today reads **only the
immediately preceding stage** (`api/v1alpha1/workflow_types.go:216-229`) — for a join
"the preceding stage" is not even well-defined. **#562 (stage-qualified `inputsFrom`)
is therefore a hard prerequisite**, not a nice-to-have; this design consumes its
resolution model and adds a branch segment to the qualifier.

#562's form is `<stage>.<outputKey>`. A branch is a *subgraph* with many stages, so the
branch name alone cannot identify whose `Outputs` supplied a key. The qualified form is
therefore four-segment — `<parallel>.<branch>.<stage>.<outputKey>` — naming the stage
exactly as #562 does:

```yaml
    - name: collate
      inputsFrom:
        securityFindings: "focus-areas.security.review-security.findingsRef"
        perfFindings:     "focus-areas.performance.review-performance.findingsRef"
```

The three-segment shorthand `<parallel>.<branch>.<outputKey>` is admitted **only** when
the branch has exactly one `@join`-terminal stage, resolving to that stage. Rule 2
permits several `@join` exits per branch, so the compiler rejects the shorthand with a
"branch has N join-terminal stages, qualify the stage" diagnostic rather than guessing.

Scalar outputs are the *only* thing `inputsFrom` can carry — `Outputs` is
schema-constrained to `string`/`number`/`integer`/`boolean`/`null`
(`api/schemas/result.schema.json:11-17`). So a branch's findings **must** travel as an
artifact, and `inputsFrom` carries only the pointer/summary scalars. That is a
pre-existing constraint, not one this design adds, and it is why the artifact union
below is the load-bearing half of fan-in.

Artifacts fan in by **union**: the join's invocation receives every branch's recorded
artifact refs, each tagged with its branch id and name. Context pointers are named
positionally today — `<stage>.artifact[i]`, accumulated into a walk-local slice
(`internal/runner/run.go:3186`) — and that accumulation order becomes nondeterministic
the moment two branches run concurrently. Since the pointer set is journaled and
digest-compared for conformance, the join's merge order is **pinned to branch
declaration order**, with each branch's own pointers in their existing within-branch
order. Deterministic, and independent of how the runner scheduled the branches.

This is the mechanism the quality-sprint lead agent reads its N reports through — by
journaled, digested pointer, inside one run, rather than by globbing a directory tree
written by N sibling runs. It is also the answer to the "can artifacts carry a tree of
markdown reports across runs?" unknown: **under this design they do not have to**,
because there are no sibling runs. (The cross-run path exists and is capability-gated
via `ContextPointer.RunID` + `journal:read`, `internal/harness/context.go:58-64` — it
is simply not needed here.)

Because branch subgraphs are disjoint (§5.5 rule 1), a branch-qualified reference is
unambiguous, and a reference to a branch that failed resolves to absent — which the
join sees in the completeness record and must handle. Unresolvable references (unknown
parallel, unknown branch, unknown output key where statically knowable) fail compile.

## 8. DSL version treatment

Two independent axes govern this, and they compose exactly as
[`dsl-version-lifecycle.md`](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/design/dsl-version-lifecycle.md) §2 describes.

**Axis 1 — interpreter version.** New constructs land in the **copy-forward
interpreter** (`internal/workflow/v_next/`, DSL `2.0`), never by editing `v_current`
(`1.4`) in place. `v_next` is today a mechanical copy of `v_current` — identical modulo
package name, the `DSLVersion` constant, and regenerated goldens — so it is the
sanctioned home for the first genuinely new language feature. A workflow using
`parallels:` pins `dslVersion: "2.0"`; a build without the constructs rejects it
through the existing "not supported by this build" path
(`internal/workflow/compile.go:184-208`) — no silent degradation.

**Axis 2 — per-feature lifecycle.** Every DSL surface element carries a `FeatureID`
with a `preview → ga → deprecated → removed` lifecycle. Static fan-out/fan-in
graduated to **GA in #1939** after the conformance corpus (§9) shipped, and the
registry now declares its DSL fields GA. Using a parallel no longer requires the
`goobers.dev/allow-preview-features` annotation.

Mechanical consequences for every slice that touches the schema: a new field must be
added in **four** hand-maintained places — the Go type with kubebuilder markers,
`api/schemas/workflow.schema.json`, the CRD under `config/crd/bases/` (`make
generate`; note the CRDs are already drifted and not CI-gated), and the feature
registry — then `FeaturesForWorkflow` must learn to detect it and
`docs/feature-matrix.md` regenerated with `make docs`.

## 9. Conformance

The construct enters the cross-runner conformance surface (§4), which means it needs
corpus fixtures before it can be claimed conformant. `make test-conformance` gains, at
minimum:

- a 3-branch parallel where every branch succeeds;
- one branch failing under each of the three failure policies, asserting both the
  routing decision and whether the join ran;
- a branch exceeding `branchTimeoutSeconds`, and one whose stage retries push it over;
- a branch stage returning `no-work`, asserting the branch completes and **the run
  does not** (§5.3) — the one place this design changes an existing status's meaning;
- a branch containing a gate that routes to `@abort`, asserting the whole run aborts
  and sibling branches are recorded `cancelled`;
- a run crashing and recovering mid-parallel with branches at different depths,
  asserting the resumed run reaches the same terminal as an uninterrupted one (R5);
- a `continue_on_error` parallel executed with `maxConcurrentBranches: 1` and with
  `> 1`, asserting both produce the same per-branch projection. Restricted to
  `continue_on_error` deliberately: under the cancelling policies the two are *expected*
  to differ (§5.3), so this fixture would be asserting a falsehood.

Each fixture asserts each branch's own `seq`-ordered event sequence plus the
completeness record — never the cross-branch interleaving, and never absolute `seq`
values across branches (§6.2).

## 10. Delivery

Each slice is independently green-and-mergeable. Land in order; FO-2 and FO-3 are the
only pair that is awkward to split further.

| Slice | Scope | Depends on | Risk |
|---|---|---|---|
| **FO-1** · Normative amendments | `ARCHITECTURE.md` §3.2/§3.3, `CFG-022`, `GAG-010`, `event.go` `Branch` comment. Docs + comments only. | this doc accepted | Low |
| **FO-2** · DSL schema + compile validation | `parallels:` types in `v_next`, JSON schema, CRD, feature-registry rows as **preview**, the `IsReservedBranchTarget` predicate (§5.2), graph projection, all ten §5.5 rules. Runner still refuses to *execute* a parallel (fail closed). | FO-1 | Low |
| **FO-3** · Multi-cursor state + journal branch semantics | Branch cursors in `State`, branch/parallel event types, `Branch` populated, and the §6.4 pass scoping every resume reconstruction helper by branch — with recovery tests, before anything executes. | FO-2 | **High** |
| **FO-4** · Branch workspaces | New `repo-readonly` `WorkspaceMode` (detached HEAD) **and** lifting `Workspace` onto the agentic task/gate path; compile-reject writable repo workspaces inside a branch (§6.5). Unblocks `maxConcurrentBranches > 1`. | FO-2 | Med |
| **FO-5** · Branch execution in the local runner | Advance N cursors, `maxConcurrentBranches`, instance `maxParallelBranches`, branch-scoped `no-work`, branch-aware `taskOutcome` in **both** the walk and `Resume`. Executes `continue_on_error` only; the other two policies **fail closed at runtime** with an explicit "not yet implemented" terminal until FO-6. | FO-3, FO-4 | **High** |
| **FO-6** · Failure policies + branch timeout | All three policies, `onFailure` routing, cooperative cancellation, completeness record. | FO-5 | Med |
| **FO-7** · Fan-in data flow | Branch-qualified `inputsFrom`, artifact union in declaration order. | FO-6, **#562** | Med |
| **FO-8** · Conformance corpus + feature GA | §9 fixtures, `make test-conformance` corpus, preview → `ga` transition, `make docs`. | FO-7 | Med |
| **FO-9** · Portal rendering | Parallel nodes and branch lanes in the run graph, on top of #1427's transition projection (whose acceptance criteria already require that parallel branches produce no cross-branch phantom edges) and after #1430 fixes edge inference. | FO-3, **#1427**, **#1430** | Low |
| **FO-10** · `quality-sprint` workflow | The driving use case, built on the primitive: churn analysis → N lenses → collate → nominate. Needs a design of its own. | FO-8 | Med |

## 11. Non-goals

- **Dynamic / data-driven branch width.** Branch count fixed at author time. Runtime
  DAG expansion is a materially different problem (admission, bounding, capability
  containment) and belongs with #817's self-authoring work. Static is the load-bearing
  half either way: the join, the failure policies, the branch journal semantics, and
  the completeness record are all unchanged by how N is determined.
- **Nested parallels.** One level. Removes the state-model and cancellation-tree
  complexity for no use case we have. Revisit on evidence.
- **Child workflows.** Stays a tier-3 extension; #155 retains this half.
- **Cross-branch communication.** Branches are disjoint by construction (§5.5 rule 1).
  Branches that need to talk are one branch.
- **Partial/quorum joins** ("proceed on the first 3 of 5"). `continue_on_error` plus a
  join that reads the completeness record covers the real cases without a second
  synchronisation primitive.
- **Human gates inside a branch** (§5.5 rule 10). Suspending one branch for days while
  siblings hold workspaces is a new suspension model. Human gates before the parallel
  or at/after the join work unchanged.
- **Concurrent code-writing branches.** Writable repo workspaces inside a parallel are
  compile-rejected (§6.5). Fanning out *edits* needs a merge policy at the join that
  we have no evidence for; fanning out *research* does not. The first form of this
  primitive is deliberately "fan out read-only work, fan in findings."
- **Parallelising existing shipped workflows.** No shipped definition changes here.

## 12. Risks

- **R1 — Multi-cursor state is the blast radius.** Every run reader, the portal, the
  telemetry projection, and recovery assume one cursor. *Mitigation:* `MachineState`
  is preserved and additive (§6.1); FO-3 lands the state change with recovery tests
  before any branch actually executes (FO-5).
- **R2 — Cancellation semantics under `fail_fast` will disappoint someone.** "Cancel"
  means "start no more stages," not "kill the running agent." *Mitigation:* stated in
  §5.3 and asserted by a conformance fixture, so the contract is discovered at
  authoring time rather than in an incident.
- **R3 — Conformance-surface promotion binds the future Temporal runner.** We are
  specifying semantics on the local runner that tier 3 must then match. *Mitigation:*
  this is deliberate (§4) — the fixtures are the spec, and branch-order independence
  (§6.2) leaves the Temporal mapping room to schedule as it likes.
- **R4 — Fan-in is gated on #562.** If #562 slips, FO-7 slips. *Mitigation:* FO-1..FO-6
  deliver working fan-out/fan-in control flow without it; only the *data* handoff
  waits, and the artifact union is independently useful.
- **R5 — Resume divergence is the likeliest real bug.** Branch routing must land
  identically in the live walk and in `Resume`, and every backward-scan helper must be
  branch-scoped (§6.4). A miss here produces a run that behaves differently after a
  restart than it did before one — silent, and invisible outside a crash.
  *Mitigation:* a conformance fixture that crashes and recovers mid-parallel with
  branches at different depths is mandatory in FO-8, and FO-3 carries recovery tests
  ahead of execution.
- **R6 — The workspace constraint could be discovered late.** If FO-5 were attempted
  before FO-4, concurrent repo-backed branches would fail on a git worktree collision
  that reads as a mysterious runtime error. *Mitigation:* FO-4 is sequenced before
  execution and closes the door at compile time (§5.5 rule 9).
- **R7 — Weakening `seq` normativity is a conformance-contract change.** §6.2 makes
  absolute `seq` non-comparable across branches. Done carelessly this could mask a real
  ordering regression in *sequential* workflows. *Mitigation:* the relaxation applies
  only between distinct non-zero branch ids; within a branch, and for every run that
  never forks, `seq` comparison is bit-for-bit what it is today.

## 13. Success criteria

This design has succeeded when all of the following are true:

1. A single workflow definition expresses churn-analysis → N named research lenses →
   collate → nominate, and one `goobers run` of it produces **one** run id, **one**
   journal, and **one** budget — replacing the N+2-definition construction of §2.
2. Removing a lens is a one-block edit to one file, and adding a lens whose branch has
   no path to `@join` fails `goobers validate` rather than hanging a live run.
3. A run whose third lens fails under `continue_on_error` still produces a collated
   artifact from the other lenses, and the join can *name* the failed lens from the
   completeness record rather than inferring absence.
4. Killing the daemon mid-parallel and restarting reaches the same terminal phase and
   the same completeness record as an uninterrupted run.
5. `make test-conformance` covers §9's fixture list, and `docs/feature-matrix.md` lists
   the new constructs at `ga`.

## 14. Open questions

- **OQ-1 — Resolved.** Static fan-out/fan-in graduated to GA in #1939 and no longer
  requires the preview-feature annotation (§8).
- **OQ-2 — Default `maxConcurrentBranches`.** *(Recommend: 1 — a parallel is about
  graph shape and join semantics first; concurrency is an opt-in performance
  decision, and defaulting to sequential makes the first implementation's journal
  trivially deterministic and sidesteps §6.5 entirely for early adopters.)*
- **OQ-3 — Branch cap.** §5.5 proposes 32. *(Recommend: 32, host-declared rather than
  schema-pinned, so it can move without a DSL bump.)*
- **OQ-4 — Does `fail_fast` need a cancellation grace period** for stages that could
  checkpoint rather than be discarded at the boundary? *(Recommend: no for FO-6 —
  discard is simpler and correct; revisit if a real stage wants it.)*
