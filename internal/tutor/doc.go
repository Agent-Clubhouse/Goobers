// Package tutor implements the Goobers self-improvement loop: it mines run,
// task, and gate telemetry for recurring problems, turns those signals into
// config-as-code improvement proposals, and opens pull requests through the
// provider abstraction.
//
// Superseded by design — pre-architecture code with zero importers (#125): it
// consumes in-memory OTel spans production never populates, and has no §11
// disposition row. The real T1 tutor design lives in
// docs/design/v1/36-tutor-workflow.md; delete this package outright once T1
// (#101) lands rather than trying to reconcile it with that design.
package tutor
