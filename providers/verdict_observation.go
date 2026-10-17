package providers

// VerdictReadStatus records whether a successful poll established the shape of
// every native combined-status response. It is observation-only provenance.
type VerdictReadStatus string

// Native status readers set complete only for explicit, non-null status arrays
// whose entries have a context and a recognized provider state.
const (
	VerdictReadComplete   VerdictReadStatus = "complete"
	VerdictReadIncomplete VerdictReadStatus = "incomplete"
)

func verdictReadStatus(complete bool) VerdictReadStatus {
	if complete {
		return VerdictReadComplete
	}
	return VerdictReadIncomplete
}

func validObservedStatus(context, state string) bool {
	if context == "" {
		return false
	}
	switch state {
	case "success", "failure", "error", "pending":
		return true
	default:
		return false
	}
}

func pullRequestPollFailure(phase string, err error) (PullRequestPollResult, error) {
	return PullRequestPollResult{PollErrorPhase: phase}, err
}
