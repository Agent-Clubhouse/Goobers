package readmodel

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// RequiredMCPCondition is categorical session evidence, scoped so one stage's
// recovery cannot clear a different stage's failure. ObservedAt is evidence
// time, not list-read time. Active never means a provider root cause is known.
type RequiredMCPCondition struct {
	Adapter       string    `json:"adapter"`
	Server        string    `json:"server"`
	Stage         string    `json:"stage"`
	Branch        int       `json:"branch"`
	ObservedAt    time.Time `json:"observedAt"`
	Category      string    `json:"category"`
	Connection    string    `json:"connection"`
	Inventory     string    `json:"inventory"`
	Authorization string    `json:"authorization"`
	Active        bool      `json:"active"`
	Reason        string    `json:"reason,omitempty"`
}

const MaxRequiredMCPConditions = 64

// RequiredMCPState is persisted in the existing operator-facts JSON, so list
// consumers never reopen run journals. Nil means no supported observation.
// Truncated prevents a bounded sample being advertised as complete coverage.
type RequiredMCPState struct {
	Conditions []RequiredMCPCondition `json:"conditions"`
	Truncated  bool                   `json:"truncated,omitempty"`
}

func (current *RequiredMCPState) After(event journal.Event) *RequiredMCPState {
	next, ok := requiredMCPObservation(event)
	if !ok {
		return current
	}
	state := RequiredMCPState{}
	if current != nil {
		state = *current
		state.Conditions = slices.Clone(current.Conditions)
	}
	for i, prior := range state.Conditions {
		if sameRequiredMCPContext(prior, next) {
			state.Conditions[i] = MergeRequiredMCPCondition(prior, next)
			return &state
		}
	}
	if len(state.Conditions) >= MaxRequiredMCPConditions {
		state.Truncated = true
		return &state
	}
	state.Conditions = append(state.Conditions, MergeRequiredMCPCondition(RequiredMCPCondition{}, next))
	return &state
}

func requiredMCPObservation(event journal.Event) (RequiredMCPCondition, bool) {
	var result RequiredMCPCondition
	if !event.KnownSchema() || event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != "required-mcp-readiness" || event.Time.IsZero() || len(event.Stage) > 256 {
		return result, false
	}
	data, err := json.Marshal(event.Runner)
	if err != nil {
		return result, false
	}
	var payload struct {
		SchemaVersion int `json:"schemaVersion"`
		RequiredMCPCondition
	}
	if json.Unmarshal(data, &payload) != nil || payload.SchemaVersion != 1 {
		return result, false
	}
	result = payload.RequiredMCPCondition
	if result.Server != "goobers-io" || (result.Adapter != "copilot-cli" && result.Adapter != "claude-code") {
		return result, false
	}
	if !validMCPObservationStatus(result.Connection) || !validMCPObservationStatus(result.Inventory) || !validMCPObservationStatus(result.Authorization) {
		return result, false
	}
	switch result.Category {
	case "ready":
		if result.Connection != "ready" || result.Inventory != "ready" || result.Authorization != "ready" {
			return result, false
		}
	case "check_unobservable", "transport_failure", "required_tool_unavailable", "authentication_failure", "tool_authorization_failure":
	default:
		return result, false
	}
	result.Stage, result.Branch, result.ObservedAt = event.Stage, event.Branch, event.Time
	result.Active, result.Reason = false, "" // Never trust producer-supplied derived state.
	return result, true
}

func sameRequiredMCPContext(a, b RequiredMCPCondition) bool {
	return a.Adapter == b.Adapter && a.Server == b.Server && a.Stage == b.Stage && a.Branch == b.Branch
}

// MergeRequiredMCPCondition folds already-validated observations for one scoped
// context. Fleet consumers sort by ObservedAt and reuse this rule across runs;
// a newer unsupported authorization check cannot clear an older denial.
func MergeRequiredMCPCondition(prior, next RequiredMCPCondition) RequiredMCPCondition {
	if next.ObservedAt.Before(prior.ObservedAt) {
		return prior
	}
	switch next.Category {
	case "ready":
		next.Active, next.Reason = false, ""
	case "check_unobservable":
		next.Active, next.Reason = prior.Active, prior.Reason
		if next.Connection == "ready" && next.Inventory == "ready" && (prior.Reason == "transport_failure" || prior.Reason == "required_tool_unavailable") {
			next.Active, next.Reason = false, ""
		}
	default:
		next.Active, next.Reason = true, next.Category
	}
	return next
}

func validMCPObservationStatus(value string) bool {
	return value == "ready" || value == "unobservable" || value == "denied"
}
