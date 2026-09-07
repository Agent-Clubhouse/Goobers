package readmodel

import (
	"encoding/json"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// EngineFallback is an observed routing decision, not a prediction about a
// definition that has not run yet. Identity includes the actual dispatch run.
type EngineFallback struct {
	Gaggle            string    `json:"gaggle"`
	Workflow          string    `json:"workflow"`
	RunID             string    `json:"runId"`
	At                time.Time `json:"at"`
	Reason            string    `json:"reason"`
	ReasonClass       string    `json:"reasonClass"`
	PlacementDeclared bool      `json:"placementDeclared"`
	SelfPinnedStages  []string  `json:"selfPinnedStages,omitempty"`
	UnpinnedGates     []string  `json:"unpinnedGates,omitempty"`
}

// After reduces a new event without discarding an earlier routing decision
// when subsequent annotations describe unrelated runner activity.
func (current *EngineFallback) After(event journal.Event) *EngineFallback {
	if value, ok := RunnerEngineFallback(event); ok {
		return value
	}
	return current
}

// RunnerEngineFallback is shared by instance and run projections. Older
// annotations retain their evidence but are explicitly unclassified.
func RunnerEngineFallback(event journal.Event) (*EngineFallback, bool) {
	if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != journal.RunnerAnnotationEngineSelection {
		return nil, false
	}
	data, err := json.Marshal(event.Runner)
	if err != nil {
		return nil, false
	}
	var result EngineFallback
	if json.Unmarshal(data, &result) != nil {
		return nil, false
	}
	result.Gaggle, result.Workflow, result.RunID, result.At = event.Gaggle, event.Workflow, event.RunID, event.Time
	if result.ReasonClass == "" {
		result.ReasonClass = "unknown"
	}
	return &result, true
}
