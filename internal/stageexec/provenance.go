package stageexec

// AgentProvenance identifies the configured model and preflighted harness
// version for spans emitted before an agent executor is resolved or invoked.
type AgentProvenance struct {
	Model          string
	HarnessVersion string
}
