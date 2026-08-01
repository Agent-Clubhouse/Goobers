package workflow

// CheckPathSimulation reports inputsFrom handoffs that cannot resolve on some
// concrete path through the workflow (#913).
func CheckPathSimulation(def Definition) []string {
	interpreter, err := interpreterForDefinition(def)
	if err != nil {
		return []string{err.Error()}
	}
	return interpreter.checkPathSimulation(def)
}
