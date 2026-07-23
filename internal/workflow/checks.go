package workflow

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
)

// CheckWarnings reports non-fatal workflow diagnostics.
func CheckWarnings(def Definition) []string {
	return vcurrent.CheckWarnings(def)
}

// CheckReachability reports unreachable states and loops with no exit.
func CheckReachability(def Definition) []string {
	return vcurrent.CheckReachability(def)
}

// CheckSchedules reports invalid schedule expressions.
func CheckSchedules(def Definition) []string {
	return vcurrent.CheckSchedules(def)
}

// CheckTriggerFields reports invalid trigger-specific field combinations.
func CheckTriggerFields(def Definition) []string {
	return vcurrent.CheckTriggerFields(def)
}

// CheckWorkflowAdmission reports capability and harness violations.
func CheckWorkflowAdmission(def Definition, goobers map[string]apiv1.GooberSpec) []string {
	return vcurrent.CheckWorkflowAdmission(def, goobers)
}

// CheckGateParameters reports invalid built-in gate parameters.
func CheckGateParameters(def Definition) []string {
	return vcurrent.CheckGateParameters(def)
}

// CheckGateOutcomes reports invalid or uncovered gate outcomes.
func CheckGateOutcomes(def Definition) []string {
	return vcurrent.CheckGateOutcomes(def)
}
