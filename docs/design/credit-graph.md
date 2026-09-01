# Credit graph contract

> Status: **implemented — contract, provenance capture, and read model landed for #4077**

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
recorded call. Each one also appends a `Gap` naming what is absent. The read
model never infers a link to close a hole, so a partially instrumented run
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
