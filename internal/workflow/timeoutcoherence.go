package workflow

// CheckStageTimeoutCoherence reports waits that can outlive their stage.
func CheckStageTimeoutCoherence(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkStageTimeoutCoherence(def)
}

// CheckSubprocessTimeoutCoherence reports a deterministic stage whose command
// wraps a subprocess carrying its own, longer wall-clock ceiling than the
// stage's own budget (#3377).
func CheckSubprocessTimeoutCoherence(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkSubprocessTimeoutCoherence(def)
}
