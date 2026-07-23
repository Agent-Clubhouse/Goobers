package workflow

import vcurrent "github.com/goobers/goobers/internal/workflow/v_current"

// CheckStageRequiredInputs reports missing inputs for known built-in commands.
func CheckStageRequiredInputs(def Definition) []string {
	return vcurrent.CheckStageRequiredInputs(def)
}
