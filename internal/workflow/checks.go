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
	return interpreter.checkWorkflowAdmission(def, goobersForCapabilityAdmission(goobers))
}

// CheckPushBoundaries reports provably cross-platform task transitions that
// hand off unpushed repo workspace state (#2861). gaggleRequiredCapabilities
// is the workflow's gaggle-level GaggleSpec.RequiredCapabilities; each
// stage's effective requirement set is that union its own.
func CheckPushBoundaries(def Definition, gaggleRequiredCapabilities []string) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkPushBoundaries(def, gaggleRequiredCapabilities)
}

// CheckRunsOnOSTokens reports os=* capability tokens in a document — CAP004
// (dsl-3.0.md D12). Only the 3.0 interpreter can produce findings; earlier
// versions have no runsOn surface to scan.
func CheckRunsOnOSTokens(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkRunsOnOSTokens(def, gaggleRunsOn)
}

// CheckRunsOnRestrictions reports restriction tokens outside the closed v1
// effect list, with did-you-mean suggestions — CAP005 (dsl-3.0.md §5).
func CheckRunsOnRestrictions(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkRunsOnRestrictions(def, gaggleRunsOn)
}

// CheckRunsOnPlacement reports structural runsOn problems on a 3.0 document
// (invalid os enum, malformed quantity, gaggle-vs-stage OS conflict, removed
// 2.0 surface) — and, on a pre-3.0 document, any use of the 3.0-only surface
// (runsOn/repoFrom/commitsRepo, or a gaggle runsOn floor), which those frozen
// interpreters must never learn.
func CheckRunsOnPlacement(def Definition, gaggleRunsOn *apiv1.GaggleRunsOn) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkRunsOnPlacement(def, gaggleRunsOn)
}

// CheckRepoHandoffs reports undeclared, mis-covered, or dead repoFrom
// repo-handoff declarations — WF022 (dsl-3.0.md §4), computed as reaching
// definitions over the stage graph.
func CheckRepoHandoffs(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkRepoHandoffs(def)
}

// CheckGateRunsOn reports the gate-only runsOn rules on a 3.0 document —
// WF023 (decision 001, dsl-3.0.md §2 "Gates"): runsOn on a non-agentic gate,
// and an agentic gate runsOn without cpu and memory. On a pre-3.0 document
// the field itself is refused by CheckRunsOnPlacement, so this reports
// nothing.
func CheckGateRunsOn(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkGateRunsOn(def)
}

// CheckGatePlacementWarnings reports WF024 on a 3.0 document: one warning per
// agentic gate that declares runsOn while no execution path honours a gate
// placement (decision 001 rulings 7–8 unlanded — the reviewer still
// evaluates in the control plane; a placement self cannot satisfy is refused
// at start). A pre-3.0 document cannot carry the field, so this reports
// nothing there. Retires with the engine half.
func CheckGatePlacementWarnings(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkGatePlacementWarnings(def)
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
