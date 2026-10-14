package executor

import (
	"reflect"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestProviderReadScopeRequiresExplicitHostAutomationOptIn(t *testing.T) {
	env := apiv1.InvocationEnvelope{Gaggle: "own", ConfigGeneration: "pinned-generation"}
	forged := []string{"PATH=/bin", ProviderReadBindingEnvVar + "=automation", ProviderReadGenerationEnvVar + "=other-generation"}
	for _, test := range []struct {
		name       string
		executor   ShellExecutor
		context    bool
		generation string
		want       []string
	}{
		{name: "unknown or human executor", context: true, generation: env.ConfigGeneration, want: []string{"PATH=/bin"}},
		{name: "ordinary CLI", executor: ShellExecutor{AutomationReadCache: true}, context: true, generation: env.ConfigGeneration, want: []string{"PATH=/bin", ProviderReadBindingEnvVar + "=automation", ProviderReadGenerationEnvVar + "=pinned-generation"}},
		{name: "build subprocess", executor: ShellExecutor{AutomationReadCache: true}, generation: env.ConfigGeneration, want: []string{"PATH=/bin"}},
		{name: "unpinned legacy invocation", executor: ShellExecutor{AutomationReadCache: true}, context: true, want: []string{"PATH=/bin"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			env.ConfigGeneration = test.generation
			got := test.executor.providerReadEnvironment(forged, env, test.context)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatal(got, test.want)
			}
		})
	}
}
