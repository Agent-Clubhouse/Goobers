# Credit graph contract

> Status: **implemented — contract, provenance capture, and read model landed
> for #4077; credit propagation and failure-cause classification for #4078**

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
`evaluator`.

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
joined to another span's call. The read model never infers a link to close a
hole, so a partially instrumented run
projects a smaller, honest graph instead of a complete-looking, fabricated one.

## Provenance capture

The graph is built from what a run already emits: `run.started`/`run.finished`,
`stage.started`/`stage.finished`, `agent.lifecycle` (which already carries
`id`, `parentId`, `dependsOn`, stage, attempt, and resolved model),
`artifact.recorded`, `gate.evaluated`/`gate.overridden`, and `span.recorded`
transcript spans in the `goobers.dev/telemetry/genai-event/v1` shape, whose
records supply model invocations, tool calls, and tool results.

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
