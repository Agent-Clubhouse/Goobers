package runsemantics

import (
	"fmt"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"strings"
)

// BranchInputSource is the settled branch state used by both execution lanes.
// Completed contains the addressable outputs and their original provenance.
type BranchInputSource struct {
	Start     string
	Status    journal.BranchStatus
	Completed StageOutputs
}

// BranchInputResolution keeps a bound value and its provenance together.
// Recognized distinguishes a branch reference from legacy dotted scalar keys.
// Absent means the branch produced no consumable result; the binding is omitted.
type BranchInputResolution struct {
	Value                     any
	Integrity                 apiv1.Integrity
	Found, Recognized, Absent bool
}

// ResolveBranchInput applies the runner's branch-qualified input contract to a
// frozen fan-in view. Missing keys on successful branches remain errors; failed,
// cancelled, timed-out, empty and skipped branches contribute no value or grade.
func ResolveBranchInput(value string, machine *workflow.Machine, parallel string, branches map[string]BranchInputSource) BranchInputResolution {
	ref, ok := splitBranchInput(value)
	if !ok || parallel == "" || ref.parallel != parallel {
		return BranchInputResolution{}
	}
	resolved := BranchInputResolution{Recognized: true}
	branch, found := branches[ref.branch]
	if !found {
		return resolved
	}
	switch branch.Status {
	case journal.BranchFailed, journal.BranchTimedOut, journal.BranchCancelled, journal.BranchNoOutput, journal.BranchSkipped:
		resolved.Absent = true
		return resolved
	}
	if ref.stage == "" {
		ref.stage = singleJoinTerminalTask(machine, branch.Start)
	}
	produced, seen := branch.Completed[ref.stage]
	if seen {
		resolved.Value, resolved.Found = produced.Outputs[ref.key]
		resolved.Integrity = produced.Integrity
	}
	return resolved
}

// BranchInputsFromError phrases a missing branch output identically on both lanes.
func BranchInputsFromError(taskName, inputKey, value string) error {
	return fmt.Errorf("task %q: inputsFrom %q: branch output %q not found", taskName, inputKey, value)
}

type branchInputRef struct {
	parallel string
	branch   string
	stage    string
	key      string
}

func splitBranchInput(value string) (branchInputRef, bool) {
	parts := strings.SplitN(value, ".", 4)
	switch len(parts) {
	case 3:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return branchInputRef{}, false
		}
		return branchInputRef{parallel: parts[0], branch: parts[1], key: parts[2]}, true
	case 4:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
			return branchInputRef{}, false
		}
		return branchInputRef{parallel: parts[0], branch: parts[1], stage: parts[2], key: parts[3]}, true
	default:
		return branchInputRef{}, false
	}
}

func singleJoinTerminalTask(machine *workflow.Machine, start string) string {
	seen := map[string]bool{}
	stack := []string{start}
	var terminal string
	for len(stack) > 0 {
		state := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if state == workflow.TerminalComplete || workflow.IsReservedAnyTarget(state) || seen[state] || !machine.Has(state) {
			continue
		}
		seen[state] = true
		if task, ok := machine.Task(state); ok && task.Next == workflow.TargetJoin {
			if terminal != "" {
				return ""
			}
			terminal = state
		}
		stack = append(stack, machine.Outgoing(state)...)
	}
	return terminal
}
