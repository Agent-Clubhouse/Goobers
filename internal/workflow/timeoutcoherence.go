package workflow

// CheckStageTimeoutCoherence reports waits that can outlive their stage.
func CheckStageTimeoutCoherence(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkStageTimeoutCoherence(def)
}
