package main

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

const legacyFailAmbiguous = "legacy-fail-ambiguous"

func publishedVerdictReason(v apiv1.Verdict) string {
	if v.Decision == apiv1.VerdictFail && v.ReasonCode == "" {
		return legacyFailAmbiguous
	}
	return string(v.ReasonCode)
}

func orderingDeferralVerdict(v apiv1.Verdict) apiv1.Verdict {
	if v.Decision != apiv1.VerdictNeedsChanges || !sequencingOnly(v.Findings) {
		return v
	}
	v.Decision = apiv1.VerdictDefer
	v.ReasonCode = apiv1.VerdictReasonOrdering
	v.Elected = false
	if v.Rationale == "" {
		v.Rationale = "Landing is deferred until sibling ordering permits this candidate to proceed."
	}
	return v
}
