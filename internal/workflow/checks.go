package workflow

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// CheckWarnings reports non-fatal workflow diagnostics.
func CheckWarnings(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkWarnings(def)
}

// CheckReachability reports unreachable states and loops with no exit.
func CheckReachability(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkReachability(def)
}

// CheckSchedules reports invalid schedule expressions.
func CheckSchedules(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkSchedules(def)
}

// CheckTriggerFields reports invalid trigger-specific field combinations.
func CheckTriggerFields(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkTriggerFields(def)
}

// CheckWorkflowAdmission reports capability and harness violations.
func CheckWorkflowAdmission(def Definition, goobers map[string]apiv1.GooberSpec) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkWorkflowAdmission(def, goobers)
}

// CheckGateParameters reports invalid built-in gate parameters.
func CheckGateParameters(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkGateParameters(def)
}

// CheckGateOutcomes reports invalid or uncovered gate outcomes.
func CheckGateOutcomes(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkGateOutcomes(def)
}
