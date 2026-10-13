package executor

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Provider read partitions are a trusted executor-to-CLI protocol. These values
// are not credentials or permission; provider operations still resolve their
// declared capability's credential before attempting a cached read.
const (
	ProviderReadBindingEnvVar    = "GOOBERS_PROVIDER_READ_BINDING"
	ProviderReadGenerationEnvVar = "GOOBERS_PROVIDER_READ_GENERATION"
)

func (e *ShellExecutor) providerReadEnvironment(values []string, env apiv1.InvocationEnvelope, injectRunContext bool) []string {
	// Neither ambient passthrough nor authored run.env can opt a launch in.
	cleaned := make([]string, 0, len(values)+2)
	for _, value := range values {
		name, _, _ := strings.Cut(value, "=")
		if name != ProviderReadBindingEnvVar && name != ProviderReadGenerationEnvVar {
			cleaned = append(cleaned, value)
		}
	}
	if e.AutomationReadCache && injectRunContext && env.Gaggle != "" && env.ConfigGeneration != "" {
		cleaned = append(cleaned, ProviderReadBindingEnvVar+"=automation", ProviderReadGenerationEnvVar+"="+env.ConfigGeneration)
	}
	return cleaned
}
