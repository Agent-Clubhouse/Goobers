package gate

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

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
	if !exhausted || v == nil || !StructuredMechanicalEscalation(g) {
		return v
	}
	copy := MechanicalVerdict(*v, apiv1.VerdictReasonRepassBudget, true)
	if copy.Rationale == "" {
		copy.Rationale = "The configured repass budget is exhausted; the review findings remain unresolved."
	}
	return &copy
}
