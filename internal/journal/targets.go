package journal

// Reserved gate targets the journal must recognize to reconstruct a run's
// phase from its event log.
//
// These duplicate workflow.TargetAbort / workflow.TargetEscalate /
// workflow.TargetJoin by VALUE rather than importing them: internal/journal is
// a leaf the workflow interpreters and the runner all depend on, so importing
// internal/workflow here would invert that layering. The duplication is pinned
// by a conformance test (targets_conformance_test.go) that fails if the
// interpreter's constants ever drift from these.
const (
	// TargetAbort ends a run as aborted.
	TargetAbort = "@abort"
	// TargetEscalate ends a run as needing human intervention.
	TargetEscalate = "@escalate"
	// TargetJoin ends a parallel BRANCH, not the run. It is reserved but must
	// never be treated as a terminal phase signal: the run continues at the
	// parallel's join state once every branch settles.
	TargetJoin = "@join"
)
