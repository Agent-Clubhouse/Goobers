package gate

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

func (e *Evaluator) setEvidenceInspectionVerdict(g apiv1.Gate, result *Result) error {
	if !StructuredMechanicalEscalation(g) {
		return nil
	}
	verdict := *result.Verdict
	if e.RecoveryVerdict != nil {
		prior, err := e.RecoveryVerdict(g.Name)
		if err != nil {
			return err
		}
		if prior != nil {
			mechanicalReason := verdict.Rationale
			verdict = *prior
			verdict.Rationale += "\n\nMechanical stop: " + mechanicalReason
		}
	}
	verdict = MechanicalVerdict(verdict, RemediationVerdictReason(result.Reason), true)
	result.Verdict = &verdict
	result.Outcome = string(verdict.Decision)
	return nil
}

// RemediationVerdictReason maps runner validation reasons onto the closed
// verdict reason-code taxonomy.
func RemediationVerdictReason(reason string) apiv1.VerdictReasonCode {
	if reason == ReasonRemediationFindingsUnaccounted {
		return apiv1.VerdictReasonFindingsUnaccounted
	}
	return apiv1.VerdictReasonEvidenceNotInspected
}

func (e *Evaluator) setInterruptedVerdict(g apiv1.Gate, result *Result) error {
	if !StructuredMechanicalEscalation(g) {
		return nil
	}
	var prior *apiv1.Verdict
	if e.RecoveryVerdict != nil {
		var err error
		prior, err = e.RecoveryVerdict(g.Name)
		if err != nil {
			return err
		}
	}
	verdict := apiv1.Verdict{}
	if prior != nil {
		verdict = *prior
	}
	if verdict.Rationale == "" {
		verdict.Rationale = "The interrupted review exhausted its repass budget; no prior reviewer rationale was recorded."
	}
	verdict = MechanicalVerdict(verdict, apiv1.VerdictReasonRepassBudget, true)
	result.Outcome = string(verdict.Decision)
	result.Verdict = &verdict
	return nil
}

// StructuredMechanicalEscalation reports whether this gate opts into the
// expanded disposition vocabulary and supplies a separate mechanical route.
// Legacy gates retain their existing synthesized verdicts.
func StructuredMechanicalEscalation(g apiv1.Gate) bool {
	_, deferral := g.Branches[string(apiv1.VerdictDefer)]
	_, escalation := g.Branches[string(apiv1.VerdictEscalate)]
	return deferral && escalation
}

// MechanicalVerdict preserves the complete synthesized explanation while
// distinguishing an opted-in mechanical stop from substantive rejection.
func MechanicalVerdict(v apiv1.Verdict, reason apiv1.VerdictReasonCode, enabled bool) apiv1.Verdict {
	if enabled {
		v.Decision = apiv1.VerdictEscalate
		v.ReasonCode = reason
		v.Elected = false
	}
	return v
}

// BudgetEscalationVerdict preserves the review which exhausted the policy
// budget, instead of turning its findings into a substantive rejection.
func BudgetEscalationVerdict(g apiv1.Gate, exhausted bool, v *apiv1.Verdict) *apiv1.Verdict {
	_, hasEscalation := g.Branches[string(apiv1.VerdictEscalate)]
	if !exhausted || v == nil || v.Decision != apiv1.VerdictNeedsChanges || !hasEscalation {
		return v
	}
	copy := MechanicalVerdict(*v, apiv1.VerdictReasonRepassBudget, true)
	stop := "The run stopped because the configured repass budget was exhausted; these findings remain actionable and were not judged inherently unresolvable."
	if copy.Rationale == "" {
		copy.Rationale = stop
	} else {
		copy.Rationale += "\n\nMechanical stop: " + stop
	}
	return &copy
}
