package runner

import "github.com/goobers/goobers/internal/invoke"

// RefuseExecution disables local entry points and both executor factories.
// The caller supplies host-owned admission policy, never workflow settings.
func RefuseExecution(cfg Config, reason error) Config {
	if reason == nil {
		return cfg
	}
	cfg.ExecutionRefusal = reason
	cfg.NewAgentic = func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return nil, reason }
	cfg.NewDeterministic = func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return nil, reason }
	return cfg
}
