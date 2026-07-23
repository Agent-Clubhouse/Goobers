package workflow

// CheckStageRequiredInputs reports missing inputs for known built-in commands.
func CheckStageRequiredInputs(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkStageRequiredInputs(def)
}
