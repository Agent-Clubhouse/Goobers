# Credit graph contract

> Status: **implemented for per-run attribution and cohort evidence.** The graph
> contract and provenance capture landed for #4077, credit propagation and
> failure-cause classification for #4078, and the read service now calls
> `creditgraph.Build` and `creditgraph.Attribute` while constructing stored
> attribution observations for EffectiveVersion/workload cohorts.
> Delivered-by: #4077, #4078
>
> The older `internal/readmodel/credit.go` implementation remains a separate
> cross-run operational ranking. See
> [Relationship to `internal/readmodel`](#relationship-to-internalreadmodel)
> for the boundary between these paths. Their remaining reconciliation is
> tracked by
> [#4523](https://github.com/Agent-Clubhouse/Goobers/issues/4523).

## Workflow enrollment

Backprop is opt-in per workflow on DSL 3.0:

```yaml
dslVersion: "3.0"
spec:
  backprop:
    enabled: true
    version: v1
```

Omitting `backprop`, or setting `enabled: false`, performs no attribution work.
For an enrolled workflow, terminalization durably appends `run.finished` and
then writes `attribution.json` beside the run journal. The record pins the workflow identity and digest, EffectiveVersion,
workload, run ID, contract version, deterministic attribution, and an explicit
`complete`, `insufficient-evidence`, or `failed` analysis status. Analysis is
read-only and best effort: a failure is reported independently and never changes
the run's phase, gate verdict, CI admission, or publication path.
EffectiveVersion includes the pinned workflow and goober digests plus the
recorded model and harness version; readers use that persisted value unchanged
for every analysis status so incompatible runs cannot enter the same cohort.
Runs containing more than one model/harness pair have no defined
EffectiveVersion and are excluded from cohort aggregation.

## Why

Credit assignment needs one shared answer to "what produced this outcome, and
through which nested execution element?". Today the journal records the pieces
— runs, stages, nested agent lifecycles, transcript spans, artifacts, gate
verdicts — but nothing joins them into a graph a consumer can traverse, and
nothing states which joins the journal does *not* support. Without that second
half, a consumer silently guesses, and a guessed edge is indistinguishable from
a recorded one once it lands in an aggregate.

## Contract

`internal/creditgraph` defines the graph and materializes it for one run.

**Nodes** (`NodeKind`): `outcome`, `run`, `stage`, `subagent`,
`model-invocation`, `tool-call`, `tool-result`, `tool`, `evidence`,
`evaluator`, `runtime`, `environment`.

**Edges** (`EdgeKind`), always pointing from the containing or causing element
to the nested or caused one: `attributed-to` (outcome → run), `contains`,
`delegates` (subagent → subagent), `invokes` (model invocation → tool call),
`uses` (tool call → shared tool identity), `produces` (tool call → tool result,
stage → evidence), `evaluates` (evaluator → stage), `depends-on`.

The root is the run's final outcome, so `Graph.Walk` from `Graph.RootID`
traverses downward from the outcome into every nested execution element. A
node reachable from several parents — the shared `tool` identity is the normal
case — is visited once per traversal, so the graph is a DAG, not a tree.

**Provenance.** Every node and edge carries `ProvenanceRecorded` or
`ProvenanceUnknown`. Unknown means the element is referenced by something the
journal recorded but has no record of its own: a stage that never journaled
`stage.started`, an agent named as a parent that never journaled a lifecycle, a
span whose content is unavailable, a tool result whose call id matches no
recorded call *in its own span*. Each one also appends a `Gap` naming what is
absent. Per-record nodes — model invocations, tool calls, tool results — are
identified by the owning span's content digest as well as the owner and record
index, so a subagent that emits several spans (one per stage attempt, say)
keeps each span's records distinct and a call id reused across spans is never
joined to another span's call. The projection never infers a link to close a
hole, so a partially instrumented run
projects a smaller, honest graph instead of a complete-looking, fabricated one.
("Read model" in this document's earlier revisions meant this journal→graph
projection, not `internal/readmodel`; the two are unrelated, which is exactly
the confusion #4523 exists to remove.)

## Provenance capture

The graph is built from what a run already emits: `run.started`/`run.finished`,
`stage.started`/`stage.finished`, `agent.lifecycle` (which already carries
`id`, `parentId`, `dependsOn`, stage, attempt, and resolved model),
`artifact.recorded`, `gate.evaluated`/`gate.overridden`, and `span.recorded`
transcript spans in the `goobers.dev/telemetry/genai-event/v1` shape, whose
records supply model invocations, tool calls, and tool results.
Recorded telemetry span attributes also project harness versions as `runtime`
nodes and deployment identities as `environment` nodes. Missing attributes do
not invent components; when present, each component retains the exact
`span.recorded` journal sequence and span artifact reference used as evidence.

The one link the journal did not carry is *which subagent a transcript span
belongs to*. The harness executor now appends an additive `runner.annotation`
of kind `credit-span-provenance` naming the span digest and the stage's single
root nested agent. It is runner-namespace bookkeeping, excluded from
conformance, and it is emitted only when that root is unambiguous — otherwise
the span's owner stays an explicitly unknown subagent node rather than a
guessed one.

## Consuming the graph

```go
graph, err := creditgraph.Build(creditgraph.Input{
    RunID: runID, Gaggle: gaggle, Workflow: workflow,
    Events: events, SpanData: spanContentByDigest,
})
graph.Walk(graph.RootID, func(node creditgraph.Node, depth int) bool { ... })
```

`SpanData` is optional: a caller that cannot cheaply read span content gets the
run's structure with the model and tool layer reported as gaps. Nodes, edges,
and gaps are in deterministic construction order, so two builds of the same
journal compare equal.

## Credit propagation

`creditgraph.Attribute` computes the credit assignment over a built graph
(#4078). It answers two questions, and refuses to answer either where the
journal is silent.

**Signed contribution with uncertainty.** Responsibility starts as one unit of
mass at the outcome and is split down the graph's edges, weighting an edge
higher when the child's own recorded signal agrees with the outcome's direction
and lower when it disagrees. Each `Contribution` reports the resulting `Share`
in [0,1] and a `Score` that carries the direction the node is estimated to have
pushed: a tool result that succeeded inside a failing run scores positive, the
failing stage scores negative. `Uncertainty` rises with unrecorded nodes,
unknown-provenance edges, gaps on the node, and unrecorded elements below it,
and `Confidence` is its complement. Nodes are emitted in graph order and every
number is rounded, so two attributions of one graph compare equal.

**Failure-cause classification.** For each stage the journal recorded as
failing, `Attribute` emits `CauseFinding`s drawn from a fixed taxonomy:
`bad-tool-choice`, `bad-tool-result`, `bad-interpretation`,
`weak-instructions`, `routing`, `model`, `topology`, `environment`, and
`unknown`. Each finding names the node it attributes, the assumptions the rule
rests on, and the evidence behind it. Contradictory signals — a passing
evaluator verdict on a failing stage, successful tool results beside a failing
one — lower a finding's confidence and are recorded as assumptions rather than
resolved by fiat. A repeated stage attempt whose outcome differed is recorded
as `intervention:` evidence and is the only entry that is more than
correlational.

Missing provenance never becomes blame: a stage whose own record is absent,
whose subtree is mostly unrecorded, or whose only work is behind a gap yields a
single `unknown` finding with reduced confidence, and a run that failed with no
failing stage yields an `unknown` finding on the outcome rather than one pinned
on whichever stage is present.

## Relationship to `internal/readmodel`

Two packages compute "which node contributed to bad outcomes", with distinct
production responsibilities.

| | `internal/creditgraph` | `internal/readmodel` (`credit.go`, `causal.go`) |
|---|---|---|
| Landed | #4077 (2026-08-31), #4078 (2026-09-02) | #2957 (2026-08-15), extended by #3545 |
| Scope | One run: a typed DAG projected from that run's journal, plus signed contribution, uncertainty, and a failure-cause taxonomy | Cross-run: a SQLite rollup over `run_node`/`run`, ranking nodes by routed/failure/escalation/retry-waste counts in a time window |
| Provenance model | Explicit `ProvenanceRecorded`/`ProvenanceUnknown` plus `Gap` records; refuses to infer a missing link | Counts what the projection recorded; no per-edge provenance concept |
| Production callers | `internal/readservice/telemetry_attribution.go` reconstructs stored runs with `Build`, computes `Attribute`, and feeds cohort aggregation and evidence surfaces | `goobers telemetry query --aggregate credit-assignment` (`cmd/goobers/telemetryquery.go`), the read API (`internal/readservice/telemetry.go`), and the portal through it |

So today:

- **`internal/readmodel` remains the aggregate operational ranking path.**
  Existing credit-assignment queries and portal summaries continue to use its
  SQLite rollup.
- **`internal/creditgraph` is the per-run evidence path.** The read service
  reconstructs selected stored runs, computes provenance-aware attribution,
  and aggregates those observations into the cohort and contributing-path
  surfaces.

### What remains

#4523 remains open for the remaining architectural work: define compatibility
and migration between the operational rollup and provenance-aware per-run
attribution, and add conformance coverage for overlapping answers. It no longer
tracks missing production wiring for `Build` or `Attribute`.
