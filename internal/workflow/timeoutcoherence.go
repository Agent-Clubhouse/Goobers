package workflow

import vcurrent "github.com/goobers/goobers/internal/workflow/v_current"

// CheckStageTimeoutCoherence reports waits that can outlive their stage.
func CheckStageTimeoutCoherence(def Definition) []string {
	return vcurrent.CheckStageTimeoutCoherence(def)
}
