package workflow

import vcurrent "github.com/goobers/goobers/internal/workflow/v_current"

// CheckStageContracts reports stage output/input contract violations.
func CheckStageContracts(def Definition) []string {
	return vcurrent.CheckStageContracts(def)
}

// CheckStageContractWarnings reports non-breaking stage contract findings.
func CheckStageContractWarnings(def Definition) []string {
	return vcurrent.CheckStageContractWarnings(def)
}
