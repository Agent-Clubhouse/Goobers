// Package gate implements gate execution (issue #20, docs/ARCHITECTURE.md §5,
// docs/requirements/gate.md): the automated and agentic reviewer evaluators, and
// the bounded-repass loop that lets a gate bounce a run back to an earlier stage
// a limited number of times before escalating.
//
// This package is substrate-neutral: it depends only on api/v1alpha1, the
// invoke seam (internal/invoke), and the compiled machine (internal/workflow) —
// none of it is Temporal- or local-runner-specific, so both runners
// (docs/ARCHITECTURE.md §3.1, §3.2) can drive gates through it. Journaling goes
// through the small Journal interface in journal.go, satisfied directly by
// *internal/journal.Run — the caller wires in a real journal (or a test double)
// without this package importing runner internals.
//
// AutomatedEvaluator (automated.go) implements invoke.Automated against a
// deliberately small, documented expression surface over the invocation
// envelope's Inputs — see automated.go for the checker registry and the
// convention the runner MUST follow when building an automated gate's
// InvocationEnvelope (propagate the subject stage's Status and small Outputs
// into Inputs). ReviewerEvidence (reviewer.go) builds the evidence-pointer
// context for an agentic reviewer gate. Evaluator (evaluate.go) is the
// orchestrator: it dispatches by evaluator kind, resolves the outcome to a
// branch via workflow.BranchTarget, enforces the bounded-repass budget, and
// journals the verdict.
package gate
