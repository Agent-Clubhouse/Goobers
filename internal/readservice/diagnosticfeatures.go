package readservice

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/diagnostics/featureusage"
)

// DiagnosticFeatureConfiguration projects finite configured-feature labels from
// the currently admitted definition snapshot. ReloadDefinitions atomically
// replaces that snapshot; this never reads unadmitted edits from disk. Runner
// flags are omitted because effective engine/env selection belongs to startup.
// Configured means available in admitted definitions, not observed execution.
func (s *Local) DiagnosticFeatureConfiguration() map[string]map[string]bool {
	result := map[string]map[string]bool{}
	definitions := s.definitionsForQuery()
	if definitions == nil {
		return result
	}
	for _, gaggle := range definitions.Gaggles {
		if len(result) >= 100 {
			break
		}
		features := map[string]bool{}
		for _, id := range featureusage.IDs() {
			if !strings.HasPrefix(id, "runner.") {
				features[id] = false
			}
		}
		for _, provider := range []apiv1.Provider{gaggle.Spec.Project.Provider, gaggle.Spec.Backlog.Provider} {
			configureProvider(features, provider)
		}
		for _, repo := range gaggle.Spec.AdditionalRepos {
			configureProvider(features, repo.Provider)
		}
		result[gaggle.Name] = features
	}
	for _, goober := range definitions.Goobers {
		id := ""
		switch goober.Spec.Harness {
		case "", apiv1.HarnessCopilot:
			id = "adapter.copilot"
		case apiv1.HarnessClaudeCode:
			id = "adapter.claude"
		case apiv1.HarnessCodex:
			id = "adapter.codex"
		}
		if id == "" {
			continue
		}
		if goober.Spec.Gaggle == "" {
			for _, features := range result {
				features[id] = true
			}
		} else if features := result[goober.Spec.Gaggle]; features != nil {
			features[id] = true
		}
	}
	for _, definition := range definitions.Workflows {
		if features := result[definition.Spec.Gaggle]; features != nil {
			for _, task := range definition.Spec.Tasks {
				if task.NestedAgentPolicy != nil {
					features["capability.nested-agents"] = true
				}
			}
			switch definition.DSLVersion {
			case "1.4":
				features["dsl.v1"] = true
			case "2.0":
				features["dsl.v2"] = true
			case "3.0":
				features["dsl.v3"] = true
			}
		}
	}
	return result
}
func configureProvider(features map[string]bool, provider apiv1.Provider) {
	switch provider {
	case apiv1.ProviderGitHub:
		features["provider.github"] = true
	case apiv1.ProviderGitea:
		features["provider.gitea"] = true
	case apiv1.ProviderADO:
		features["provider.azure-devops"] = true
	}
}
