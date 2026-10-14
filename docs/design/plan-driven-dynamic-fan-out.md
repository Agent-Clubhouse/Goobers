# Design: Conditional and plan-driven fan-out

> Status: **approved** — contract locked 2026-10-14; implementation proceeds by the slices in §15
> Area: workflow DSL, runner, journal, conformance
> Tracking: #1310
> Extends: [`static-fan-out-fan-in.md`](static-fan-out-fan-in.md)
> Companion: [`agent-authored-child-workflows.md`](agent-authored-child-workflows.md)
> Related: #155, #817, #2891, #4769, #4771, #5123, #6868
> Verified: 561268950 (2026-10-14)

## 1. Problem

Static `spec.parallels[].branches[]` is GA. It gives Goobers a bounded native
fan-out with a real join, but two things are fixed when the workflow is authored:

1. **Which branches run.** Every declared branch runs on every visit. A workflow
   with a security lens, a migration lens and a UI lens runs all three even when
   the change touches only the UI. There is no way to say "run this branch only
   when it applies".
2. **How many branches run.** Branch width, names and start states are fixed. A
   stage cannot pick a roster at run time and ask the runner to execute one native
   branch per entry.

Typical cases:

- review lenses that apply only to some changes (conditional branches);
- test shards selected from a pinned test inventory (data-driven width);
- migration batches selected from a versioned manifest (data-driven width); and
- research questions selected from a frozen investigation plan (data-driven width).

The workarounds lose important properties:

- An automated gate at a branch's start that routes straight to `@join` makes the
  branch a no-op, but the join records it as `succeeded` or `no-output`. It can't
  tell "did not apply" from "ran and found nothing". The decision is scattered
  across N gates rather than recorded once, and the branch still takes a
  concurrency slot.
- Static slots with placeholders need an arbitrary maximum.
- Rediscovering the roster after an interruption can produce a different roster.
- A custom dispatcher stage loses native journal, retry, timeout, cancellation,
  workspace and artifact semantics.
- A sequential generic worker loses bounded concurrency and per-item recovery.

Agent-authored child workflows (approved, largely landed in October 2026) solve
a different problem: the agent decides the whole *composition* at run time. They
are the most powerful construct and also the most expensive, since each child is a
separately admitted run with its own workspace fork, custody and wait. Conditional
and data-driven fan-out should not need that machinery. Both are an
author-declared graph whose runtime choice is a small, validated, frozen piece of
data.

## 2. Decision summary

1. **One selection contract, two constructs.** A completed task publishes a small
   JSON document. The runner validates and journals it before any branch starts,
   then never consults the producer again. *Selective parallels* use it to choose
   which declared branches run. *Maps* use it to choose how many instances of one
   declared task run.
2. **No expression language.** There is no `when:`, JSONPath, interpolation or
   predicate DSL. The condition is computed by an ordinary task: a deterministic
   script (for example, "which paths changed") or an agentic triage stage. That
   task keeps its own goober, capabilities, retry, timeout and integrity.
3. **The source is a DSL 3.1 named artifact slot** (#4771). With a JSON slot the
   producer gets publish-time validation and same-session repair for free
   (#6868, first slice in #7060). A malformed roster is caught while the planner
   can still fix it, not after it exits.
4. **Selective parallel.** A static parallel may declare `selection`. Branches
   marked `optional: true` run only when the selection names them. Non-optional
   branches always run. An unselected branch is recorded with the new terminal
   status `skipped`, never starts, and is never a failure.
5. **Map.** A new core state, `map`, runs one statically declared task once per
   roster item, through the existing parallel executor. It is not a second
   scheduler. Width is bounded by the static parallel cap (32).
6. **Frozen and durable.** The normalized selection or roster is written into the
   fan-out's start event. Resume rebuilds from that event and never reruns or
   rereads the producer.
7. **Preview in DSL 3.1, local runner only.** Both constructs are additive DSL 3.1
   preview features behind `goobers.dev/allow-preview-features`. The Temporal
   engine walk already refuses `spec.parallels`, and it refuses these too, at
   start, with a typed unsupported-feature error.
8. **Child workflows compose with these constructs. The constructs do not
   replace child workflows** (§11). Generated children may use selective
   parallels and maps under the parent's preview policy. A mapped task may not
   enable child tools in V1.

## 3. Choosing a control-flow construct

| You need | Use | Decided by | Shape fixed by the author |
| --- | --- | --- | --- |
| One path out of several | Gate `branches` (existing) | Gate verdict | All paths |
| A fixed set of branches, all run | Static `parallel` (GA) | — | Branches and width |
| A fixed set of different branches, a subset run | Selective `parallel` (§5) | Selection artifact | Branches; width ≤ declared |
| The same task over a data-determined list | `map` (§6) | Roster artifact | Task template; width ≤ 32 |
| A composition nobody can know until run time | Child workflow | The parent agent writes a Workflow | Policy ceiling only |
| Skip the whole fan-out | Gate before the parallel or map (existing) | Gate verdict | Both paths |

This guide is part of the contract. The authoring skill and composition catalog
from #7185 should point authors at the cheapest construct that fits.

## 4. The shared selection contract

### 4.1 Source

Both constructs name their source the same way:

```yaml
source:
  task: plan          # a task that precedes the fan-out on every path
  slot: assignments   # a DSL 3.1 artifactSlot declared by that task
```

The compiler requires:

- `source.task` names a task, not a gate, parallel or map. It must dominate the
  fan-out: every path from `start` to the fan-out passes through it.
- `source.slot` names an `artifactSlots[]` entry on that task with
  `mediaType: application/json`.
- The slot's `maxSize`, when declared, is at most the host roster bound (1 MiB).
  When it is omitted, the host bound applies.

**Effective slot schema.** Goobers ships two closed envelope schemas:
`api/schemas/fanout-selection.schema.json` and
`api/schemas/fanout-roster.schema.json`.

- When the source slot declares no `schemaPath`, the compiler lowers the matching
  envelope schema onto the slot as its effective schema.
- When the slot declares its own `schemaPath`, both that schema and the host
  envelope are enforced. The runner checks the author schema at publication and
  completion (#7060), and checks the envelope again when it freezes the document.

Either way, the producing agent is told about a malformed document at
`publish_output` and gets the existing bounded same-session repair turn.

### 4.2 Freezing

Before the fan-out starts any branch, the runner:

1. resolves the slot from the producer's recorded attempt (the journal-bound
   producer and digest, as #5089 specifies for consumer bindings);
2. checks media type, byte size (before decoding) and the closed envelope;
3. applies the construct-specific checks (§5.3, §6.2); and
4. appends the fan-out start event, carrying the source reference, the source's
   digest and integrity, and the complete normalized document.

No branch starts until that append succeeds. The event is the durable source of
truth for this fan-out visit. A failure in steps 1–3 is a typed
`fanout_source_invalid` stage failure of the fan-out state, which takes the
fan-out's `onFailure` route, or `@abort` when none is declared. It is never a
silent empty fan-out.

### 4.3 Integrity

The selection or roster carries the source artifact's integrity.

- **Maps:** every bound item value inherits that integrity. It takes part in the
  mapped task's `minimumIntegrity` admission and in result-integrity calculation,
  exactly like `inputsFrom` values. Resume reconstructs the integrity from the
  journaled source reference. It never defaults bound values to trusted static
  input.
- **Selective parallels:** the selection is control data. No selected branch
  receives it as input. Its integrity is recorded on the start event for audit.
  An author who must not let lower-integrity input skip a check must leave that
  branch non-optional. **`optional: true` is the author's statement that skipping
  is acceptable, and it is the only way a branch can be skipped.**

### 4.4 Re-entry

A gate that loops back so the fan-out runs again starts a new visit. The new
visit freezes whatever the producer's latest attempt published. If the loop also
re-enters the producer, that is the producer's new output. If not, the same
recorded attempt is used again. Each visit has its own start event and its own
branch occurrences (#6881), exactly as static parallels behave on re-entry today.

## 5. Selective parallel (conditional branches)

### 5.1 DSL surface

```yaml
dslVersion: "3.1"
metadata:
  annotations:
    goobers.dev/allow-preview-features: "true"
spec:
  start: triage
  tasks:
    - name: triage
      type: deterministic
      goal: Decide which review lenses apply to this change.
      run:
        command: ["lens-selector", "--json", "--out", "lenses.json"]
        workspace: repo-readonly
      artifactSlots:
        - name: lenses
          mediaType: application/json
      next: review

    - name: review-security
      type: agentic
      goober: security-reviewer
      goal: Review the change for security impact.
      workspace: repo-readonly
      next: "@join"
    # review-migration, review-ui and review-baseline each end at "@join"

    - name: collate
      type: agentic
      goober: review-lead
      goal: Collate every lens that ran; note the lenses that were skipped.
      workspace: scratch
      next: ""

  parallels:
    - name: review
      failurePolicy: continue_on_error
      branchTimeoutSeconds: 1800
      maxConcurrentBranches: 3
      join: collate
      selection:
        source: { task: triage, slot: lenses }
      branches:
        - { name: baseline,  start: review-baseline }                 # always runs
        - { name: security,  start: review-security,  optional: true }
        - { name: migration, start: review-migration, optional: true }
        - { name: ui,        start: review-ui,        optional: true }
```

The selection document:

```json
{
  "schemaVersion": 1,
  "branches": [
    { "name": "security", "reason": "touches internal/auth" },
    { "name": "ui" }
  ]
}
```

- `branches[].name` must name a declared branch of this parallel.
- `reason` is an optional human-facing string, at most 512 bytes. It is
  journaled, shown in the Portal and the join's completeness record, and excluded
  from conformance comparison.
- Naming a non-optional branch is allowed and has no effect. Non-optional
  branches always run.
- Duplicate names and unknown names are rejected at freezing.
- An empty `branches` array is valid: only the non-optional branches run.

### 5.2 Semantics

- **Branch identity is unchanged.** Numeric branch IDs are still assigned to every
  declared branch in declaration order, selected or not. One workflow therefore
  produces the same `(branch, name)` pairs whatever the selection, and existing
  branch-qualified references stay stable.
- **Skipped branches never start.** They get no `branch.started` event and no
  stage, workspace, worktree, child-workflow fork or concurrency slot. They also
  consume none of `branchTimeoutSeconds`.
- **`skipped` is a new terminal status** in the branch completeness record
  (`journal.BranchStatus`), alongside `succeeded`, `failed`, `timed-out`,
  `cancelled` and `no-output`. It means "the selection did not choose this
  branch". It is deliberately distinct from `no-output` ("ran and produced
  nothing") and `cancelled` ("chosen, then stopped").
- **Failure policies ignore skipped branches.** A skipped branch is never a
  failure. `fail_fast` cancels only selected branches that have not yet started.
  `all_or_nothing` succeeds when every *selected* branch succeeds.
- **The join runs exactly once**, even when every branch is skipped. Every
  declared branch has exactly one entry in the completeness record, skipped ones
  included.
- **Compile-time bounds are unchanged.** A parallel still declares between 2 and
  32 branches. At run time, any number from 0 to the declared count may run.

### 5.3 Freezing checks

On top of §4.2, the runner rejects unknown or duplicate names before
`parallel.started`. The normalized selection is the set of selected optional
branches, sorted by branch ID, plus their reasons. It is written to
`parallel.started` as a new optional `selection` object:
`{source, sourceDigest, integrity, selected[], reasons{}}`.

A parallel without `selection` emits exactly the events it emits today, so
existing workflows and conformance fixtures do not change.

### 5.4 Child workflows in selective parallels

A selected branch behaves exactly like a static branch, including child-capable
branches. `ParallelHasChildStage` keeps choosing isolated writable branch forks
for them, and root joins reconcile writable state as they do today. A skipped
branch creates no fork and no child.

## 6. Map (data-driven width)

### 6.1 DSL surface

```yaml
dslVersion: "3.1"
spec:
  start: plan
  tasks:
    - name: plan
      type: agentic
      goober: shard-planner
      goal: Select the immutable test-shard roster.
      workspace: repo-readonly
      artifactSlots:
        - name: shards
          mediaType: application/json
      next: run-shards

    - name: run-shard
      type: deterministic
      goal: Run one shard.
      run:
        command: ["shard-runner"]
        workspace: repo-readonly
      next: "@join"

    - name: collate
      type: agentic
      goober: test-lead
      goal: Summarize every shard result in identity order.
      workspace: scratch
      next: ""

  maps:
    - name: run-shards
      source: { task: plan, slot: shards }
      task: run-shard
      bindings:
        shard: item
        shardIdentity: identity
      failurePolicy: continue_on_error
      itemTimeoutSeconds: 1800
      maxConcurrent: 4
      join: collate
```

The roster document:

```json
{
  "schemaVersion": 1,
  "items": [
    { "identity": "unit-a", "item": { "packages": ["./internal/a/..."] } },
    { "identity": "unit-b", "item": { "packages": ["./internal/b/..."] }, "digest": "sha256:…" }
  ]
}
```

- `identity` matches `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. It is used for display
  and in branch-scoped names, so it is restricted to a safe charset.
- `item` is a JSON object.
- `digest` is **optional**. The runner always computes the canonical digest:
  SHA-256 over the RFC 8785 serialization of `{identity, item}`. A supplied
  digest must match it. This lets an agentic planner produce a valid roster
  without computing hashes, while still letting a deterministic planner commit
  to a digest.
- Bounds are 1 MiB and 32 items. Raising them later is a host-capacity change as
  long as the wire shape and validation semantics stay the same.

The mapped task is declared once in `spec.tasks`, so all existing validation
still applies to its goober, capabilities, placement, retry, timeout, inputs,
outputs and workspace. Only its owning map may enter it, and it must end at
`@join`, `@abort` or `@escalate`.

`bindings` maps invocation input names to one of the closed values `item`,
`identity` or `digest`. These are dynamic invocation inputs, not static
`Task.Inputs` configuration. A binding name must not collide with a static
input. Field selection and string interpolation are rejected.

### 6.2 Freezing checks

On top of §4.2, the runner:

1. rejects duplicate identities;
2. computes each canonical digest and verifies any supplied digest;
3. sorts items by identity, bytewise UTF-8; and
4. appends `map.started` with the source reference, digest, integrity, failure
   policy, timeout, concurrency bound and the complete normalized roster.

An empty roster is valid. The runner writes `map.started`, immediately writes
`map.finished` with an empty ordered result set, and enters the join exactly
once.

### 6.3 Identity and execution

- After sorting, items get numeric branch IDs starting at 1. The same pinned
  roster therefore produces the same `(branch, identity, digest)` triples on every
  runner, whatever the original array order or scheduling interleaving.
- Journal branch lifecycle events gain explicit `mapItemIdentity` and
  `mapItemDigest` fields. Static branch names are not overloaded with dynamic
  identities.
- The runner expands the frozen roster into the existing parallel branch
  representation:
  - every branch starts at the mapped task;
  - `maxConcurrent` becomes `maxConcurrentBranches`;
  - `itemTimeoutSeconds` becomes `branchTimeoutSeconds`; and
  - `failurePolicy`, `join` and `onFailure` keep their static parallel semantics.

  The parallel worker pool remains the only concurrency mechanism.
- **Result storage is per branch occurrence.** A map repeats one task name, so
  any store keyed only by stage name would overwrite results. Results, join
  assembly, rerun, integrity propagation and resume all key mapped results by
  branch ID and the journal stage occurrence from #6881, which already gives each
  parallel branch a distinct occurrence. Linear execution and static disjoint
  branches keep their current lookup.
- Retries repeat only the mapped task for the same frozen item. They never
  reselect, re-hash or reorder the roster. Timeouts and `fail_fast` cancellation
  keep the existing stage-boundary behavior.

## 7. Workspace boundary

- **Mapped tasks:** the effective workspace must be `scratch` or `repo-readonly`.
  The compiler rejects writable `repo` workspaces even at `maxConcurrent: 1`, so
  the contract does not depend on a concurrency knob. Writable concurrent
  branches remain #2891.
- **Selective parallels:** exactly the static parallel rules, including the
  isolated writable forks for child-capable branches (§5.4).
- Exact-SHA repo-readonly provisioning (#5123) complements both. Neither
  construct claims stronger checkout semantics than the workspace provider gives.

## 8. Journal and resume

New and extended conformance-normative events:

| Event | Change |
| --- | --- |
| `parallel.started` | Optional `selection` object (§5.3), present only for selective parallels |
| `parallel.finished` | Completeness entries may carry `skipped` and an optional `reason` |
| `map.started` | New: map name, source reference, digest, integrity, policy, timeout, bound, normalized roster |
| `map.finished` | New: map name, ordered item outcomes, routing target |
| `branch.started` / `branch.finished` | For map items, add `mapItemIdentity` and `mapItemDigest` |

The run checkpoint keeps one cursor per started branch through the existing
`BranchCursor` mechanism, extended with optional map item identity and digest.
Pending fan-out reconstruction is generalized:

1. find the latest unmatched `parallel.started` or `map.started`;
2. rebuild the branch set only from the recorded event: for a selective parallel,
   the declared branches minus the recorded skips; for a map, the recorded roster;
3. apply branch lifecycle and stage events by numeric branch ID;
4. verify every observed branch is expected, with the identity and digest the
   roster records; and
5. refuse to resume on a missing, extra, duplicate or conflicting record.

The refusal is a typed `workflow_integrity` failure. It is never repaired by
rerunning the source task.

## 9. Join inputs

- **Selective parallel:** the join gets the existing `branchCompleteness` input.
  Skipped branches appear with status `skipped`, their reason, and zero
  artifacts. Branch outputs are reached through the existing branch-qualified
  references.
- **Map:** the join gets two inputs, assembled from the journal and never from
  goroutine completion order:
  - `mapCompleteness`: one outcome per roster item, in identity order, with every
    identity exactly once, including cancelled, timed-out, failed and no-output
    items; and
  - `mapResults`: the mapped task's scalar outputs and artifact or context
    pointers, grouped by identity in the same order, read from the
    per-occurrence store (§6.3).

There is no new dynamic `inputsFrom` path syntax. Downstream stages consume the
join's declared outputs through ordinary `inputsFrom`.

## 10. Compiler validation

All of the following fail `goobers validate`.

**Shared:**

- `source.task` is missing, is not a task, or does not dominate the fan-out;
- `source.slot` is not a declared JSON slot on that task, or declares a `maxSize`
  above the host bound;
- the construct appears in a definition pinned below DSL 3.1, or without the
  preview acknowledgement while it is preview.

**Selective parallel:**

- `selection` is declared but no branch is `optional`;
- a branch is `optional` but the parallel has no `selection`;
- every other static parallel rule, unchanged.

**Map:**

- the map name collides with any task, gate, parallel or map;
- the mapped task is missing, reachable outside the map, or owned by another map
  or by a static parallel;
- the mapped task can exit anywhere other than `@join`, `@abort` or `@escalate`;
- the mapped task uses a writable repository workspace;
- the mapped task declares `childWorkflows` (§11);
- the join or failure route is missing or contradicts the policy (the static
  `onFailure` rules apply);
- timeout or concurrency values are negative or exceed host bounds;
- a map is nested in a parallel branch or another map, or a parallel is nested in
  a map;
- a binding name collides with a static task input, or a binding value is not
  `item`, `identity` or `digest`.

The schema is closed and joins the Go-to-schema structural drift guard.

## 11. Interaction with agent-authored child workflows

- **Generated children may contain selective parallels and maps.** Child
  proposals compile through the same `workflow.Compile` path at the pinned DSL
  version. They use preview features only when the parent's pinned policy allows
  preview, which is the existing proposal rule. The child proposal state bound
  (`MaxProposalStates`) counts maps as states.
- **A mapped task may not enable child tools in V1.** Child-capable stages need
  isolated writable forks and per-occurrence custody. Combining that with
  data-driven width multiplies workspace and capacity cost by the roster size,
  with no qualified evidence yet. Revisit this together with #2891.
- **Selective parallels inherit the static child rules** (§5.4).
- **Recursion is unchanged.** A generated child cannot enable child tools, so a
  map inside a child can never produce grandchildren.

## 12. Feature lifecycle and versioning

These are additive author-visible contracts in the evolvable DSL 3.1. They are
not added to frozen DSL 2.0 or 3.0. Initial feature IDs, all preview since the
release that ships slice FAN-1:

- `workflow.spec.parallels.selection`
- `workflow.spec.parallels.selection.source`
- `workflow.spec.parallels.branches.optional`
- `workflow.spec.maps`
- `workflow.spec.maps.source`
- `workflow.spec.maps.task`
- `workflow.spec.maps.bindings`
- `workflow.spec.maps.failurePolicy`
- `workflow.spec.maps.join`
- `workflow.spec.maps.onFailure`
- `workflow.spec.maps.itemTimeoutSeconds`
- `workflow.spec.maps.maxConcurrent`

Promotion to GA requires local-runner conformance, durable-resume fault
injection, and either engine-walk support or an approved waiver recorded in this
document.

## 13. Runtime support

| Runtime | Selective parallel | Map |
| --- | --- | --- |
| Local runner | Supported from FAN-2 | Supported from FAN-4 |
| Temporal engine walk | Refused at start (already refuses `spec.parallels`) | Refused at start: new `spec.maps` unsupported-feature entry in FAN-1 |

Pod-placed stages inside branches behave as they do for static parallels on the
local runner.

Until a construct's runtime slice lands, a definition that uses it compiles but
is refused at run start with a typed unsupported-feature error. It never
silently runs as a non-conditional parallel.

## 14. Conformance cases

**Selective parallel:**

1. a selection of a subset runs exactly the selected optional branches and every
   non-optional branch;
2. an empty selection runs only the non-optional branches, and the join runs
   exactly once;
3. all branches optional and none selected: no branch starts, and the join runs
   once with every entry `skipped`;
4. unknown and duplicate names fail before `parallel.started`;
5. skipped branches keep their declared branch IDs, and selected branch IDs do
   not shift;
6. `fail_fast` and `all_or_nothing` treat skipped branches as neither failed nor
   cancelled;
7. a parallel without `selection` produces a projection byte-identical to the
   current fixture; and
8. resume after `parallel.started` reconstructs the same skip set without
   rereading the source.

**Map:**

9. a three-item roster produces exactly three branches with stable identity and
   digest;
10. a different input array order produces an identical normalized roster and
    result order;
11. different completion interleavings produce an identical `map.finished`;
12. duplicate identities fail before `map.started`;
13. a malformed or mismatched supplied digest fails before `map.started`, an
    omitted digest is computed, and digest verification excludes the digest
    field itself;
14. missing, extra, duplicate and digest-conflicting resume events fail closed;
15. `maxConcurrent` bounds active workers while every item settles;
16. retry and timeout stay scoped to one item;
17. `fail_fast` records every unstarted item as cancelled;
18. an empty roster enters the join exactly once;
19. joined outputs and artifacts keep their item identity, and repeated
    mapped-task outputs stay distinct by occurrence; and
20. roster integrity flows through mapped input admission, output integrity and
    resume without upgrading provenance.

**Shared:**

21. a schema-invalid source document is rejected at `publish_output` with the
    #7060 repair contract, and an unrepaired one fails the fan-out with
    `fanout_source_invalid`; and
22. fault injection after the start event, during several branch stages, and
    after every settlement but before the finish event resumes without any
    source-stage execution.

## 15. Delivery slices

Every slice leaves a runnable tree, states which checklist items remain open,
and advertises no resume or recovery path until its production caller and
fault-injection test land in the same change.

| Slice | Scope | Depends on |
| --- | --- | --- |
| **FAN-1 Contract** | API types (`Parallel.Selection`, `Branch.Optional`, `WorkflowSpec.Maps`), closed JSON schema and CRD, envelope schemas, preview feature registry rows, compiler validation (§10), effective slot schema lowering (§4.1), graph projection, drift guard, local-runner and engine start refusal, docs. | — |
| **FAN-2 Selective parallel runtime** | Freezing (§4.2, §5.3), `skipped` status, `parallel.started.selection`, completeness and Portal display of skipped branches, resume, conformance cases 1–8 and 21–22, and lifting the local-runner refusal for selective parallels. | FAN-1 |
| **FAN-3 Map durability** | `map.started`/`map.finished`, branch item fields, cursor extension, strict reconstruction, corruption tests, per-occurrence result store. | FAN-1 |
| **FAN-4 Map execution** | Bindings, expansion into the parallel executor, join inputs, bounds, cancellation, timeout, workspace, conformance cases 9–20, fault injection, and lifting the local-runner refusal for maps. | FAN-3 |
| **FAN-5 Composition surfaces** | Child proposal admission and state counting (§11), authoring skill and composition-catalog guidance (§3), Portal map-item lineage, a reference example. | FAN-2, FAN-4 |

Engine-walk parity is not in this delivery. It follows static parallels on the
engine (#155) and gates GA, not preview.

## 16. Rejected alternatives

- **`when:` predicates on branches.** These need an expression language, a
  defined input provenance for every predicate, and deterministic re-evaluation
  on resume. A selector task already has all three and is visible in the journal.
- **The branch-start gate idiom as the official answer.** It is still legal, but
  the join can't tell a skip from an empty success, and the decision is not
  recorded in one place.
- **A filter on a map instead of selective parallels.** A map runs one
  homogeneous task. Conditional lenses are heterogeneous: different goobers,
  multi-stage branches, different capabilities.
- **Child workflows for every dynamic case.** They work, but cost a separate run,
  a workspace fork and custody per child. Reserve them for compositions that
  can't be declared ahead of time.

## 17. Non-goals

- writable concurrent repository branches (#2891);
- arbitrary nested mapped subgraphs, and nesting maps or parallels inside each
  other;
- runtime graph mutation or self-authored stage kinds (#817; generated children
  are the bounded form);
- capability, credential or placement escalation;
- unbounded item counts;
- rediscovery-based resume;
- a second scheduler; and
- general expression or JSONPath evaluation.
