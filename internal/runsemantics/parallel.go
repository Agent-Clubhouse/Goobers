package runsemantics

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// BranchCompletenessInput is the join invocation input containing one terminal
// outcome for every declared branch, in declaration order.
const BranchCompletenessInput = "branchCompleteness"

const (
	// MapCompletenessInput is the map join invocation input holding one
	// terminal outcome per roster item, in identity order.
	MapCompletenessInput = "mapCompleteness"
	// MapResultsInput is the map join invocation input holding the mapped
	// task's outputs and pointers per roster item, in identity order.
	MapResultsInput = "mapResults"
)

// MapItemResult is one ordered mapResults join entry.
type MapItemResult struct {
	Identity  string                 `json:"identity"`
	Digest    string                 `json:"digest"`
	Branch    int                    `json:"branch"`
	Status    journal.BranchStatus   `json:"status"`
	Outputs   map[string]any         `json:"outputs,omitempty"`
	Integrity apiv1.Integrity        `json:"integrity,omitempty"`
	Pointers  []apiv1.ContextPointer `json:"pointers,omitempty"`
}

// SettledBranchStatus is the shared terminal classification for a branch that
// ran to a boundary. Cancellation/skipping are explicit outcomes applied by
// the scheduler before this rule. No-output is a successful settle.
func SettledBranchStatus(timedOut, failed, noOutput, produced bool) journal.BranchStatus {
	if timedOut {
		return journal.BranchTimedOut
	}
	if failed {
		return journal.BranchFailed
	}
	if noOutput || !produced {
		return journal.BranchNoOutput
	}
	return journal.BranchSucceeded
}

// FanOutSourceInvalidCode is the typed failure of a fan-out state whose
// source document cannot be frozen. It takes the fan-out's onFailure route,
// or @abort when none is declared; it is never a silent empty fan-out.
const FanOutSourceInvalidCode = "fanout_source_invalid"
