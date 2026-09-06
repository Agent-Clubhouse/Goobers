package workflow

import (
	"fmt"
	"strings"

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

// implicitWritableWorkspaceWarnings flags stages whose omitted workspace
// selects the historical writable run-branch worktree even though their
// declaration provides no repository-mutation signal.
func implicitWritableWorkspaceWarnings(def Definition, gooberSets ...map[string]apiv1.GooberSpec) []string {
	var warnings []string
	var goobers map[string]apiv1.GooberSpec
	if len(gooberSets) > 0 {
		goobers = gooberSets[0]
	}
	for _, task := range def.Spec.Tasks {
		if task.EffectiveWorkspace() != "" {
			continue
		}
		if task.Type == apiv1.TaskAgentic {
			if !hasRepositoryMutationSignal(task.Capabilities, task.PolicyActions) {
				warnings = append(warnings, implicitWorkspaceWarning(def.Name, "task", task.Name))
			}
			continue
		}
		if task.Type == apiv1.TaskDeterministic && task.Run != nil &&
			deterministicStageAppearsReadOnly(task) &&
			!hasRepositoryMutationSignal(task.Capabilities, task.PolicyActions) {
			warnings = append(warnings, implicitWorkspaceWarning(def.Name, "task", task.Name))
		}
	}
	for _, gate := range def.Spec.Gates {
		if gate.Evaluator == apiv1.EvaluatorAgentic && gate.EffectiveWorkspace() == "" && gate.Agentic != nil {
			var capabilities, policyActions []string
			if goober, ok := goobers[gate.Agentic.Goober]; ok {
				capabilities = goober.Capabilities
				policyActions = goober.PolicyActions
			}
			if !hasRepositoryMutationSignal(capabilities, policyActions) {
				warnings = append(warnings, implicitWorkspaceWarning(def.Name, "gate", gate.Name))
			}
		}
	}
	return warnings
}

// CheckImplicitWritableWorkspaceWarnings reports advisory workspace defaults
// separately from the compiler's historical compatibility warnings.
func CheckImplicitWritableWorkspaceWarnings(def Definition, gooberSets ...map[string]apiv1.GooberSpec) []string {
	return implicitWritableWorkspaceWarnings(def, gooberSets...)
}

func implicitWorkspaceWarning(workflow, kind, stage string) string {
	return fmt.Sprintf(
		`workflow %q %s %q omits workspace; it defaults to writable workspace: repo (a run-branch worktree). Choose workspace: scratch when no repository is needed, workspace: repo-readonly when only inspecting repository contents, or workspace: repo when writable repository state is intentional`,
		workflow, kind, stage,
	)
}

func hasRepositoryMutationSignal(capabilities, policyActions []string) bool {
	for _, value := range capabilities {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "repo:push" || strings.HasSuffix(value, ":write") ||
			strings.HasSuffix(value, ":merge") || strings.HasSuffix(value, ":close") ||
			strings.HasSuffix(value, ":complete") {
			return true
		}
	}
	for _, action := range policyActions {
		action = strings.ToLower(strings.TrimSpace(action))
		if action != "" && !strings.Contains(action, "read") &&
			!strings.Contains(action, "inspect") && !strings.Contains(action, "review") {
			return true
		}
	}
	return false
}

func deterministicStageAppearsReadOnly(task apiv1.Task) bool {
	if task.Run == nil || len(task.Run.Command) == 0 {
		return false
	}
	switch strings.ToLower(task.Run.Command[0]) {
	case "go", "gofmt", "goimports", "goobers":
		return true
	default:
		return false
	}
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
