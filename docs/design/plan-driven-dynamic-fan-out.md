# Design: Plan-driven dynamic fan-out

> Status: **draft** — requires maintainer sign-off before implementation
> Area: workflow DSL, runner, journal, conformance
> Tracking: #1310
> Related proposal: [krazeelazy/Goobers#1](https://github.com/krazeelazy/Goobers/issues/1)
> Extends: [`static-fan-out-fan-in.md`](static-fan-out-fan-in.md)
> Related: #155, #817, #2891, #5123

## 1. Problem

Static `spec.parallels[].branches[]` gives Goobers a bounded native fan-out and
fan-in, but every branch name, start state, and goober identity is fixed when
the workflow is authored. A stage cannot select an immutable assignment roster
at run time and ask the runner to execute one native branch per assignment.

The missing operation is generic. Examples include:

- review lenses selected from the files and risks present in one change;
- test shards selected from a pinned test inventory;
- migration batches selected from a versioned manifest; and
- research questions selected from a frozen investigation plan.

The workarounds lose important properties:

- static slots require an arbitrary maximum and placeholder branches;
- rediscovery after interruption can produce a different roster;
- a custom dispatcher loses native journal, retry, timeout, cancellation,
  workspace, and artifact semantics; and
- a sequential generic worker loses bounded concurrency and per-item recovery.

This proposal adds a plan-driven map state. It does not allow a run to rewrite
its graph, invent capabilities, or schedule arbitrary new stage kinds.

## 2. Decision summary

1. Dynamic fan-out is a new core DSL state named `map`, available only in the
   next evolvable DSL version and initially registered as preview.
2. A map reads one immutable JSON roster artifact from a completed task. The
   entire artifact is bounded, validated, and journaled before any item starts.
3. Every item carries an author-selected identity and a content digest. The
   normalized roster is sorted by identity and is the normative execution and
   join order.
4. A map invokes one statically declared task template once per item. V1 does
   not clone an arbitrary subgraph.
5. The existing parallel executor supplies bounded concurrency, branch retry,
   timeout, cancellation, completeness, and join behavior. A map is not a
   second scheduler.
6. V1 permits only `scratch` and `repo-readonly` task workspaces. Concurrent
   writable repository branches remain #2891.
7. The frozen roster and per-item cursors are durable. Resume never reruns the
   planner or rereads its current outputs.
8. The join receives one ordered result per pinned identity and fails closed
   if the journal contains a missing, extra, duplicate, or digest-mismatched
   item.

## 3. Proposed DSL surface

The spelling is illustrative until this draft is approved. The contract, not
the exact field names, is the review target.

```yaml
dslVersion: "3.1"
spec:
  start: plan
  tasks:
    - name: plan
      type: deterministic
      goal: Select the immutable work roster.
      run:
        command: ["planner", "--json"]
        workspace: repo-readonly
      outbox: [assignments.json]
      next: review-map

    - name: review-item
      type: agentic
      goober: generic-reviewer
      goal: Review the assigned scope.
      workspace: repo-readonly
      next: "@join"

    - name: collate
      type: agentic
      goober: review-lead
      goal: Collate every mapped result in identity order.
      workspace: scratch
      next: ""

  maps:
    - name: review-map
      source:
        task: plan
        artifact: assignments.json
      task: review-item
      bindings:
        assignment: item
        assignmentIdentity: identity
        assignmentDigest: digest
      failurePolicy: continue_on_error
      itemTimeoutSeconds: 1800
      maxConcurrent: 4
      join: collate
```

`source.task` must be a task that precedes the map on every path.
`source.artifact` must name a JSON file declared in that task's `outbox`. The
artifact has this closed V1 shape:

```json
{
  "items": [
    {
      "identity": "security",
      "digest": "sha256:...",
      "item": {}
    }
  ]
}
```

The roster artifact is size-bounded before decoding. The initial host bound is
1 MiB and 32 items. A later increase is a host-capacity change so long as the
wire shape and validation semantics remain unchanged.

The mapped task is declared once in `spec.tasks` so all existing validation
continues to apply to its goober, capabilities, placement, retry, timeout,
inputs, outputs, and workspace. It may be entered only by its owning map and
must end at `@join`. A task cannot be owned by both a static parallel and a
map, or by two maps.

`bindings` maps ordinary invocation input names to one of the closed values
`item`, `identity`, or `digest`. They are dynamic inputs, not `Task.Inputs`
static author configuration. General expressions, field selection, and string
interpolation are rejected in V1.

Every bound value inherits the roster artifact's integrity. The values
participate in the mapped task's `minimumIntegrity` admission and in result
integrity calculation exactly like `inputsFrom` values. Resume reconstructs the
same integrity from the journaled roster reference; it does not default mapped
bindings to trusted static input.

## 4. Frozen roster contract

Before launching the first item, the runner:

1. resolves the declared source from the completed task's recorded artifact;
2. verifies the artifact media type, byte size, closed object shape, and item
   count do not exceed host bounds;
3. validates every entry has a non-empty string identity and digest;
4. verifies the digest is `sha256:` plus 64 lowercase hexadecimal characters;
5. rejects duplicate identities;
6. canonicalizes `{identity, item}` as RFC 8785 JSON and verifies its SHA-256
   digest (the `digest` field is deliberately outside the hashed value);
7. sorts items by identity using bytewise UTF-8 ordering; and
8. appends one `map.started` event containing the roster artifact reference,
   its integrity, and the complete normalized roster.

No item starts until the event append succeeds. The event is the durable source
of truth for the remainder of the run. The planner output is never consulted
again for this map execution, including after restart.

The item-count bound matches the static parallel width bound. The byte and item
bounds are validation and runtime-admission bounds, not append-only structures
that need a pruner.

An empty array is valid. The map writes `map.started`, immediately writes
`map.finished` with an empty ordered result set, and enters its join exactly
once.

## 5. Stable item and branch identity

Each normalized roster entry is:

```json
{
  "identity": "security",
  "digest": "sha256:...",
  "item": {}
}
```

The identity is author data and must be stable across retries and resume. The
digest commits to the canonical `{identity, item}` pair. It cannot commit to
its own `digest` field, which would make ordinary verification impossible.

Numeric branch IDs remain the journal's compact ordering key. For a map they
are assigned after identity sorting, starting at 1. Therefore the same pinned
roster produces the same `(branch, identity, digest)` triples on every runner,
regardless of original array order or scheduling interleaving.

Static branch names are not overloaded with dynamic identities. Journal
envelopes gain explicit `mapItemIdentity` and `mapItemDigest` fields on map and
branch lifecycle events. Portal and conformance consumers can distinguish a
map item from a statically declared branch without parsing a synthetic name.

## 6. Execution and scheduling

The local runner expands the frozen roster into the existing `parallelExec`
branch representation:

- branch `start` is the statically declared mapped task;
- branch `name` is the item identity for display compatibility;
- branch metadata carries the canonical item and digest;
- `maxConcurrent` maps to `maxConcurrentBranches`;
- `itemTimeoutSeconds` maps to `branchTimeoutSeconds`; and
- `failurePolicy`, `join`, and `onFailure` retain static parallel semantics.

The parallel worker pool remains the only concurrency mechanism. The map layer
performs validation, durable expansion, and typed input binding, then delegates
execution and settlement to the existing code path.

Execution results cannot use the current `stageOutputs` map unchanged because
that map is keyed only by stage name and static parallel branches are required
to have disjoint task names. A map intentionally repeats one task. The
implementation therefore introduces branch-aware result storage keyed by
`(fanOutState, branchID, stage)` for mapped execution. Join assembly, rerun,
integrity propagation, and resume reconstruction all use that key. Ordinary
linear execution and static disjoint branches retain their existing lookup
behavior.

Retries repeat only the mapped task for the same frozen item. They do not
reselect, redigest, or reorder the roster. Timeouts and fail-fast cancellation
retain the existing stage-boundary behavior.

## 7. Workspace boundary

V1 accepts mapped tasks only when their effective workspace is:

- `scratch`; or
- `repo-readonly`.

The compiler rejects writable `repo` workspaces even when `maxConcurrent` is
one. This keeps the first contract independent of scheduling width and prevents
authors from relying on behavior that becomes unsafe when concurrency changes.

Exact-SHA repo-readonly provisioning is complementary work tracked by #5123.
Until that capability lands, a map receives the same pinned repository view as
an ordinary repo-readonly task on the local runner. The map contract does not
claim stronger checkout semantics than the workspace provider supplies.

## 8. Journal and resume

Two new conformance-normative event types are required:

- `map.started`: map name, ordered roster, failure policy, timeout, and
  concurrency bound; and
- `map.finished`: map name, ordered item outcomes, and routing target.

`branch.started` and `branch.finished` continue to record each item lifecycle
and additionally carry the item's identity and digest. Stage and artifact
events already carry the numeric branch ID; conformance joins them to the
roster recorded by `map.started`. `map.started` also carries the roster
artifact's immutable reference and integrity so resume preserves provenance.

The run checkpoint retains one cursor per item through the existing
`BranchCursor` mechanism, extended with optional item identity and digest.
`pendingParallel` becomes a generic pending fan-out reconstruction:

1. find the latest unmatched `parallel.started` or `map.started`;
2. for a map, reconstruct the branch set only from the recorded roster;
3. apply branch lifecycle and stage events by numeric branch ID;
4. verify every observed item identity and digest matches the roster; and
5. refuse resume on missing, extra, duplicate, or conflicting records.

The refusal is a typed workflow-integrity failure. It is never repaired by
rerunning the source task.

## 9. Join and downstream data flow

The map join receives two normal inputs:

- `mapCompleteness`: one outcome per roster item in identity order; and
- `mapResults`: the mapped task's scalar outputs and artifact/context pointers,
  grouped by identity in the same order.

These are assembled from the journal, not from goroutine completion order.
Every expected identity appears exactly once, including cancelled, timed-out,
failed, and no-output items.

`mapResults` is assembled from the branch-aware result store. It must not read
the existing stage-name-only map, where repeated mapped executions would
overwrite one another.

V1 does not add dynamic `inputsFrom` path syntax. The join consumes the two
well-known map inputs. Downstream stages consume the join's declared outputs
through ordinary `inputsFrom`, preserving the normal data-flow model.

## 10. Compiler validation

The interpreter rejects a definition when:

- a map name collides with any task, gate, parallel, or map;
- the source task is missing or its source artifact is not declared in outbox;
- the source task does not precede the map on every path;
- the mapped task is missing, reachable outside the map, or owned elsewhere;
- the mapped task can exit anywhere except `@join`, `@abort`, or `@escalate`;
- the mapped task uses a writable repository workspace;
- the join or failure route is missing or contradictory for the policy;
- timeout or concurrency values are negative or exceed host bounds;
- a map is nested in a static parallel or another map;
- a binding name collides with a static task input; or
- a binding uses a value outside `item`, `identity`, or `digest`.

The schema is closed and must join the Go-to-schema structural drift guard in
the implementation change.

## 11. Feature lifecycle and versioning

This is an additive author-visible contract and therefore requires a new DSL
minor after 3.0. It must not be added to frozen DSL 2.0 or retrofitted into
3.0.

The initial feature IDs are preview:

- `workflow.spec.maps`
- `workflow.spec.maps.source`
- `workflow.spec.maps.task`
- `workflow.spec.maps.bindings`
- `workflow.spec.maps.failurePolicy`
- `workflow.spec.maps.join`
- `workflow.spec.maps.onFailure`
- `workflow.spec.maps.itemTimeoutSeconds`
- `workflow.spec.maps.maxConcurrent`

Preview use requires the existing
`goobers.dev/allow-preview-features: "true"` workflow annotation. Promotion to
GA requires local-runner conformance, durable-resume fault injection, and a
second runner implementation or an approved waiver.

## 12. Conformance cases

The implementation change must add fixtures for:

1. a three-item plan producing exactly three branches with stable identity and
   digest;
2. input array order differing while normalized roster and result order remain
   identical;
3. completion interleaving differing while `map.finished` remains identical;
4. duplicate identities failing before `map.started`;
5. malformed and mismatched digests failing before `map.started`, including a
   proof that digest verification excludes the digest field itself;
6. missing, extra, duplicate, and digest-conflicting resume events failing
   closed;
7. `maxConcurrent` bounding active workers while every item settles;
8. retry and timeout remaining scoped to one item;
9. fail-fast recording every unstarted item as cancelled;
10. empty input entering the join exactly once;
11. joined outputs and artifacts retaining item identity;
12. scratch and repo-readonly acceptance plus writable-repo rejection;
13. repeated mapped task outputs remaining distinct by branch-aware key; and
14. roster integrity flowing through mapped input admission, output integrity,
    and resume without upgrading provenance.

Fault-injection coverage must stop the runner after `map.started`, during
multiple item stages, and after all item settlements but before
`map.finished`, then prove resume performs no source-stage execution.

## 13. Delivery slices

Implementation should remain reviewable and preserve a runnable tree after
every slice:

1. **Contract:** new DSL interpreter, API types, closed schema, feature
   registry, graph projection, compiler validation, drift fixture, docs, and
   compile/schema tests.
2. **Durability:** journal roster/result fields, conformance projection,
   checkpoint cursor extension, strict reconstruction, and corruption tests.
3. **Local execution:** typed bindings and adaptation into `parallelExec`,
   including bounds, cancellation, timeout, workspace, and resume tests.
4. **Cross-runner parity:** conformance fixtures consumed by every supported
   engine. Until this lands, the feature remains preview and unsupported by
   engines that cannot execute it.

Each slice must state which checklist items remain open. No slice may advertise
resume or a recovery path until its production caller and fault-injection test
land in the same change.

## 14. Non-goals

- writable concurrent repository branches (#2891);
- arbitrary nested mapped subgraphs;
- nested maps or maps inside static parallels;
- runtime graph mutation or self-authored stage kinds (#817);
- capability, credential, or placement escalation;
- unbounded item counts;
- rediscovery-based resume;
- a second scheduler; and
- general expression or JSONPath evaluation.
