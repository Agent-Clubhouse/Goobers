// Package creditgraph defines the hierarchical execution/credit graph
// contract and materializes it for one run from the journal.
//
// The graph joins the elements a credit assignment has to reason about —
// the final outcome, the run of a workflow, its stages, nested subagents,
// model invocations, tool calls and their results, the evidence produced,
// and the evaluators that judged it — into a single directed graph whose
// root is the run's final outcome, so an analysis can traverse downward
// from "what happened" to "which nested execution element contributed".
//
// The one rule the read model never breaks is that missing provenance is
// represented, never invented. A journal that does not record a link is
// projected as a node or edge with ProvenanceUnknown plus a Gap explaining
// what is absent; no link is inferred to fill the hole. A partially
// instrumented run therefore produces a smaller, honest graph rather than a
// complete-looking, fabricated one.
package creditgraph
