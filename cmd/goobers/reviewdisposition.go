package main

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

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

func terminalVerdictRequirement(v apiv1.Verdict) error {
	if v.Decision != apiv1.VerdictFail {
		return nil
	}
	if v.ReasonCode == "" {
		if hasActionableOrOrderingFinding(v.Findings) {
			return fmt.Errorf("terminal fail verdict with actionable or ordering findings requires explicit reasonCode %q", apiv1.VerdictReasonUnsalvageableDesign)
		}
		return nil
	}
	if v.ReasonCode == apiv1.VerdictReasonUnsalvageableDesign {
		if !unsalvageableDesignRationale(v.Rationale) {
			return fmt.Errorf("unsalvageable-design fail verdict requires rationale explaining why ordinary code changes cannot repair the approach")
		}
		return nil
	}
	if v.ReasonCode == apiv1.VerdictReasonImplementationRejected || v.ReasonCode == apiv1.VerdictReasonPolicyRejected {
		if hasActionableOrOrderingFinding(v.Findings) {
			return fmt.Errorf("terminal fail verdict with actionable or ordering findings requires reasonCode %q and rationale explaining why ordinary code changes cannot repair the approach", apiv1.VerdictReasonUnsalvageableDesign)
		}
		return nil
	}
	return fmt.Errorf("terminal fail verdict with reasonCode %q is not allowed for this decision/finding combination; use needs-changes or defer for fixable or ordering findings", v.ReasonCode)
}

func unsalvageableDesignRationale(rationale string) bool {
	s := strings.ToLower(strings.TrimSpace(rationale))
	if s == "" {
		return false
	}
	if !strings.Contains(s, "ordinary code changes") && !strings.Contains(s, "code changes") {
		return false
	}
	if !strings.Contains(s, "cannot") && !strings.Contains(s, "can't") && !strings.Contains(s, "not") && !strings.Contains(s, "never") && !strings.Contains(s, "impossible") {
		return false
	}
	if !strings.Contains(s, "repair") && !strings.Contains(s, "remediat") && !strings.Contains(s, "fix") && !strings.Contains(s, "salvage") && !strings.Contains(s, "restore") && !strings.Contains(s, "recover") {
		return false
	}
	return true
}

func hasActionableOrOrderingFinding(findings []apiv1.Finding) bool {
	for _, finding := range findings {
		switch finding.Class {
		case apiv1.FindingCrossPRBlocked, apiv1.FindingRebaseNeeded, apiv1.FindingConflict,
			apiv1.FindingSubstantive, apiv1.FindingMissingTests, apiv1.FindingScopeCreep, apiv1.FindingContractChange:
			return true
		}
	}
	return false
}

// reviewGateVerdictFromPublished turns a published merge-review verdict back
// into the reviewer outcome the review gate routes (#6061). apply-verdict
// publishes an ordering-only needs-changes as "defer" (orderingDeferralVerdict,
// or a typed no-lander deferral), and gather-sibling-context replays the
// published verdict as the gate's cache hit. The DSL 2.0 review gate has no
// defer branch, so a replayed "defer" failed the run closed (GT-002) without a
// model call. Replaying it as needs-changes with the same findings sends it
// through elect-lander and apply-verdict, which deterministically re-derive the
// deferral (or crown the PR if the cluster has moved).
func reviewGateVerdictFromPublished(v apiv1.Verdict) apiv1.Verdict {
	if v.Decision != apiv1.VerdictDefer {
		return v
	}
	v.Decision = apiv1.VerdictNeedsChanges
	v.ReasonCode = ""
	v.Elected = false
	return v
}
