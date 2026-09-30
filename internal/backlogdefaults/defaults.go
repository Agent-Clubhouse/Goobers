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
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	// AssignedToInput is the backlog-query input carrying the claim identity.
	AssignedToInput = "assignedTo"
	// RequireLabelsInput is the backlog-query input carrying the required
	// label list (the claim partition).
	RequireLabelsInput = "requireLabels"
	// LabelPredicateInput is the backlog-query input carrying the label CEL
	// predicate.
	LabelPredicateInput = "labelPredicate"
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

// ApplyBacklogScope conjoins the gaggle backlog label selector with a
// backlog-query or backlog-health task's own selector. Unlike RequireLabels,
// spec.backlog.labels and spec.backlog.labelPredicate are not task defaults:
// they scope the gaggle's backlog itself, so task-local inputs can only narrow
// them, never replace them.
func ApplyBacklogScope(task apiv1.Task, inputs map[string]string, backlogLabels, backlogLabelPredicate string) map[string]string {
	if backlogLabels == "" && backlogLabelPredicate == "" {
		return inputs
	}
	if !isBacklogQueryOrHealth(task) {
		return inputs
	}
	resolved := cloneInputs(inputs)
	if labels := splitLabelList(backlogLabels); len(labels) > 0 {
		resolved[RequireLabelsInput] = joinLabels(uniqueSortedLabels(append(labels, splitLabelList(resolved[RequireLabelsInput])...)))
	}
	if backlogLabelPredicate != "" {
		resolved[LabelPredicateInput] = LabelPredicateConjunction(backlogLabelPredicate, resolved[LabelPredicateInput])
	}
	return resolved
}

// ApplyBacklogScopeToInvocation conjoins the gaggle backlog label selector
// onto a dispatched invocation's already-resolved inputs. It is the post-
// inputsFrom companion to ApplyBacklogScope: upstream bindings may replace the
// task-local selector, but this step adds the gaggle scope back so they cannot
// widen eligibility.
func ApplyBacklogScopeToInvocation(task apiv1.Task, inputs map[string]interface{}, backlogLabels, backlogLabelPredicate string) (map[string]interface{}, error) {
	if backlogLabels == "" && backlogLabelPredicate == "" {
		return inputs, nil
	}
	if !isBacklogQueryOrHealth(task) {
		return inputs, nil
	}
	selectorInputs := map[string]string{}
	for _, key := range []string{RequireLabelsInput, LabelPredicateInput} {
		value, ok := inputs[key]
		if !ok || value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("%s input must be string when applying gaggle backlog scope, got %T", key, value)
		}
		selectorInputs[key] = text
	}
	scoped := ApplyBacklogScope(task, selectorInputs, backlogLabels, backlogLabelPredicate)
	resolved := make(map[string]interface{}, len(inputs)+2)
	for key, value := range inputs {
		resolved[key] = value
	}
	if value, ok := scoped[RequireLabelsInput]; ok {
		resolved[RequireLabelsInput] = value
	}
	if value, ok := scoped[LabelPredicateInput]; ok {
		resolved[LabelPredicateInput] = value
	}
	return resolved, nil
}

// LabelPredicateConjunction returns a CEL expression requiring every non-empty
// expression to match. Empty-string expressions mean "not configured"; blank
// whitespace is preserved so the downstream compiler still fails closed.
func LabelPredicateConjunction(expressions ...string) string {
	terms := make([]string, 0, len(expressions))
	for _, expression := range expressions {
		if expression == "" {
			continue
		}
		terms = append(terms, "("+expression+")")
	}
	return strings.Join(terms, " && ")
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

func cloneInputs(inputs map[string]string) map[string]string {
	resolved := make(map[string]string, len(inputs)+2)
	for key, value := range inputs {
		resolved[key] = value
	}
	return resolved
}

func splitLabelList(value string) []string {
	parts := strings.Split(value, ",")
	labels := make([]string, 0, len(parts))
	for _, part := range parts {
		if label := strings.TrimSpace(part); label != "" {
			labels = append(labels, label)
		}
	}
	return labels
}

func uniqueSortedLabels(labels []string) []string {
	seen := make(map[string]struct{}, len(labels))
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func joinLabels(labels []string) string {
	return strings.Join(labels, ",")
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
