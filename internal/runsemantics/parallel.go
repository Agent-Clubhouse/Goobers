package runsemantics

import (
	"github.com/goobers/goobers/internal/journal"
)

// BranchCompletenessInput is the join invocation input containing one terminal
// outcome for every declared branch, in declaration order.
const BranchCompletenessInput = "branchCompleteness"

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
