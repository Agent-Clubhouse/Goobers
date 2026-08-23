package goobernetes

// AssertionResult is the common return shape every assertion helper in this
// package produces: ClassifyItem's Verdict, a human-readable Detail
// (populated on anything but a clean pass), and the raw Evidence the caller
// can attach to an ObserverResult via NewObserverResult. Centralizing the
// shape means every helper's output slots into a Bundle identically,
// regardless of which surface it read.
type AssertionResult struct {
	Verdict  Verdict
	Detail   string
	Evidence any
}

// pass builds a clean AssertionResult carrying evidence but no detail
// message (ClassifyItem never populates Reason for VerdictPass).
func pass(evidence any) AssertionResult {
	return AssertionResult{Verdict: VerdictPass, Evidence: evidence}
}

// fail builds a fail AssertionResult: the observer worked, the criterion did
// not hold.
func fail(detail string, evidence any) AssertionResult {
	return AssertionResult{Verdict: VerdictFail, Detail: detail, Evidence: evidence}
}

// invalid builds an invalid AssertionResult: the observer machinery itself
// could not establish evidence.
func invalid(detail string, evidence any) AssertionResult {
	return AssertionResult{Verdict: VerdictInvalid, Detail: detail, Evidence: evidence}
}

// classify is the shared entry point every helper below funnels its raw
// boolean/precondition determination through, so ClassifyItem's rule (§5
// rule 2 / D4) is applied uniformly rather than re-implemented per helper.
// failDetail is used only when the outcome is a genuine fail: ClassifyItem
// itself never explains WHY observed is false (it only decides invalid vs.
// pass vs. fail), so each caller supplies its own specific fail message.
func classify(precondition PreconditionFailure, observed bool, failDetail string, passEvidence, failEvidence any) AssertionResult {
	verdict, reason := ClassifyItem(precondition, observed)
	switch verdict {
	case VerdictInvalid:
		return invalid(reason, failEvidence)
	case VerdictPass:
		return pass(passEvidence)
	default:
		return fail(failDetail, failEvidence)
	}
}
