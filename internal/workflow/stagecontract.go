package workflow

// CheckStageContracts reports stage output/input contract violations.
func CheckStageContracts(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkStageContracts(def)
}

// CheckStageContractWarnings reports non-breaking stage contract findings.
func CheckStageContractWarnings(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkStageContractWarnings(def)
}
