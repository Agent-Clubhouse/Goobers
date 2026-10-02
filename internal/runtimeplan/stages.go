package runtimeplan

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// StageSettings preserves execution inputs, rather than claiming that a static
// report measured a running attempt's effective deadline or allocated workspace.
type StageSettings struct {
	Workspace            string        `json:"workspace,omitempty"`
	TimeoutSeconds       int32         `json:"timeoutSeconds,omitempty"`
	Limits               *apiv1.Limits `json:"limits,omitempty"`
	GooberTimeoutSeconds int32         `json:"gooberTimeoutSeconds,omitempty"`
	LegacyTimeout        string        `json:"legacyTimeout,omitempty"`
	OnTimeout            string        `json:"onTimeout,omitempty"`
	Source               Source        `json:"source"`
}

// ResolveStages reports each task and gate's configured runtime settings.
func ResolveStages(spec apiv1.WorkflowSpec, goobers map[string]apiv1.GooberSpec) map[string]StageSettings {
	result := make(map[string]StageSettings)
	source := Source{"static", "compiled stage timeout and workspace inputs; instance/repository defaults apply when unset; parent cancellation remains runtime-only"}
	for _, task := range spec.Tasks {
		result[task.Name] = StageSettings{Workspace: string(task.Workspace), TimeoutSeconds: task.TimeoutSeconds, Limits: task.Limits, GooberTimeoutSeconds: goobers[task.Goober].TimeoutSeconds, LegacyTimeout: task.Inputs["timeout"], OnTimeout: task.OnTimeout, Source: source}
	}
	for _, gate := range spec.Gates {
		settings := StageSettings{Source: source}
		if gate.Agentic != nil {
			settings.TimeoutSeconds = gate.Agentic.TimeoutSeconds
			settings.GooberTimeoutSeconds = goobers[gate.Agentic.Goober].TimeoutSeconds
		}
		if gate.Automated != nil {
			settings.TimeoutSeconds = gate.Automated.TimeoutSeconds
		}
		if gate.Human != nil {
			settings.TimeoutSeconds = gate.Human.TimeoutSeconds
			settings.OnTimeout = gate.Human.OnTimeout
		}
		result[gate.Name] = settings
	}
	return result
}
