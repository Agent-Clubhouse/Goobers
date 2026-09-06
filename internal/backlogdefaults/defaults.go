// Package backlogdefaults injects a gaggle's claim-partition defaults into a
// `goobers backlog-query` stage's inputs.
//
// WHAT THIS IS. The local runner has applied these two defaults before every
// backlog-query dispatch since #1820/#1901 (internal/runner/run.go:4413-4414,
// over defaultBacklogQueryAssignedTo / defaultBacklogQueryRequireLabels at
// run.go:4328-4374). They are the mechanism that enforces the MIRC-2 claim
// partition: two instances — the cloud one and the laptop one — share a
// backlog repo and split it by label (`goobers:cloud` / `goobers:local`), and
// the gaggle's RequireLabels plus the instance's own identity are what keep a
// run on its own side of that line. A driver that walks the same lane without
// this defaulting does not fail; it silently claims the sibling instance's
// items (#3873).
//
// WHY IT IS A COPY AND NOT A MOVE. Decision 005 ruling 1 says type-1/type-2
// instances "keep internal/runner unmodified — that is the one remaining
// driver split and it is inherent, not a design choice", and finding 002's
// critic named the consequence: hoisting these two functions out of run.go
// would itself be a modification of internal/runner, behaviour-preserving or
// not, and would spend the one deliberate runner edit the ruling allows on a
// refactor. So this package carries a COPY, the runner's own functions and
// call sites are untouched byte for byte, and the two implementations are
// held together behaviourally rather than by sharing a symbol:
//
//   - internal/engine's parity harness compares the two drivers over the real
//     shipped lanes, and its E1 rows (parity_row_backlog_query_defaults_test.go,
//     parity_row_backlog_query_partition_test.go,
//     parity_row_backlog_query_declared_inputs_test.go) assert the RUNNER
//     exhibits each behaviour as an ungraded premise and the engine matches it.
//     A drift in either copy turns those rows red;
//   - defaults_test.go here mirrors internal/runner/selfidentity_test.go and
//     internal/runner/requirelabels_test.go case for case.
//
// The duplication is deliberate and load-bearing. Deleting it is a runner
// edit, and that edit needs the ruling's blessing, not a refactor commit.
package backlogdefaults

import (
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	// AssignedToInput is the backlog-query input carrying the claim identity.
	AssignedToInput = "assignedTo"
	// RequireLabelsInput is the backlog-query input carrying the required
	// label list (the claim partition).
	RequireLabelsInput = "requireLabels"
)

// Apply injects both gaggle defaults into a backlog-query task's inputs, in
// the order the local runner's dispatchTask applies them
// (internal/runner/run.go:4413-4414). It is the whole surface a driver needs:
// a caller that applies only one of the two has half a partition.
//
// It returns inputs unchanged — the same map, not a copy — when neither
// default applies.
func Apply(task apiv1.Task, inputs map[string]string, assignedTo, requireLabels string) map[string]string {
	inputs = AssignedTo(task, inputs, assignedTo)
	return RequireLabels(task, inputs, requireLabels)
}

// AssignedTo injects the instance's self identity (#1820, COORD-2) into a
// backlog-query task's assignedTo input: a task that already declares its own
// assignedTo wins untouched, an empty default is a no-op, and only
// `goobers backlog-query` tasks are affected.
//
// Copy of internal/runner/run.go's defaultBacklogQueryAssignedTo — see the
// package comment for why it is a copy.
func AssignedTo(task apiv1.Task, inputs map[string]string, assignedTo string) map[string]string {
	if assignedTo == "" {
		return inputs
	}
	if _, overridden := inputs[AssignedToInput]; overridden {
		return inputs
	}
	if !IsBacklogQuery(task) {
		return inputs
	}
	resolved := make(map[string]string, len(inputs)+1)
	for key, value := range inputs {
		resolved[key] = value
	}
	resolved[AssignedToInput] = assignedTo
	return resolved
}

// RequireLabels injects a gaggle's RequireLabels default (MIRC-2, #1901) into
// a backlog-query OR backlog-health task's requireLabels input, mirroring
// AssignedTo exactly: a task that already declares its own requireLabels wins
// untouched (full replace, never merged — the same override shape
// BranchNamespace/headPrefix already has), and an empty default is a no-op.
//
// backlog-health is included alongside backlog-query (#4180): both read the
// same partitioned backlog, and an engine-dispatched health check without
// this default would drift from a runner-dispatched one, which does receive
// it (internal/runner/run.go's defaultBacklogQueryRequireLabels).
//
// Copy of internal/runner/run.go's defaultBacklogQueryRequireLabels — see the
// package comment for why it is a copy.
func RequireLabels(task apiv1.Task, inputs map[string]string, requireLabels string) map[string]string {
	if requireLabels == "" {
		return inputs
	}
	if _, overridden := inputs[RequireLabelsInput]; overridden {
		return inputs
	}
	if !isBacklogQueryOrHealth(task) {
		return inputs
	}
	resolved := make(map[string]string, len(inputs)+1)
	for key, value := range inputs {
		resolved[key] = value
	}
	resolved[RequireLabelsInput] = requireLabels
	return resolved
}

// IsBacklogQuery reports whether task runs the `goobers backlog-query`
// subcommand. It is the runner's own check (AssignedTo's predicate — assigned
// identity is a claim-path concept, not a health-check one), kept as one
// function here because the two copies above must never disagree about which
// stages a default applies to.
func IsBacklogQuery(task apiv1.Task) bool {
	return isBacklogCommand(task, "backlog-query")
}

// isBacklogQueryOrHealth reports whether task runs `goobers backlog-query` or
// `goobers backlog-health` — RequireLabels' predicate, matching
// internal/runner/run.go's defaultBacklogQueryRequireLabels exactly.
func isBacklogQueryOrHealth(task apiv1.Task) bool {
	return isBacklogCommand(task, "backlog-query") || isBacklogCommand(task, "backlog-health")
}

func isBacklogCommand(task apiv1.Task, name string) bool {
	return task.Run != nil && len(task.Run.Command) >= 2 &&
		filepath.Base(task.Run.Command[0]) == "goobers" &&
		task.Run.Command[1] == name
}
