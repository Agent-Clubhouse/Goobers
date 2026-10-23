package runner

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
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
		resolved.Value, resolved.Found = produced.outputs[ref.key]
		resolved.Integrity = produced.integrity
	}
	return resolved
}

// BranchInputsFromError phrases a missing branch output identically on both lanes.
func BranchInputsFromError(taskName, inputKey, value string) error {
	return branchInputsFromError(taskName, inputKey, value)
}
