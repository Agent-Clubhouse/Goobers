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
	Adapter                 string    `json:"adapter"`
	Server                  string    `json:"server"`
	Stage                   string    `json:"stage"`
	Branch                  int       `json:"branch"`
	ObservedAt              time.Time `json:"observedAt"`
	Category                string    `json:"category"`
	Connection              string    `json:"connection"`
	Inventory               string    `json:"inventory"`
	Authorization           string    `json:"authorization"`
	Active                  bool      `json:"active"`
	Reason                  string    `json:"reason,omitempty"`
	AvailabilityObservedAt  time.Time `json:"availabilityObservedAt,omitzero"`
	AvailabilityReason      string    `json:"availabilityReason,omitempty"`
	AuthorizationObservedAt time.Time `json:"authorizationObservedAt,omitzero"`
	AuthorizationReason     string    `json:"authorizationReason,omitempty"`
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
	return decisiveMCPObservation(result), true
}

func sameRequiredMCPContext(a, b RequiredMCPCondition) bool {
	return a.Adapter == b.Adapter && a.Server == b.Server && a.Stage == b.Stage && a.Branch == b.Branch
}

// MergeRequiredMCPCondition folds already-validated observations for one scoped
// context. Fleet consumers sort by ObservedAt and reuse this rule across runs;
// a newer unsupported authorization check cannot clear an older denial.
func MergeRequiredMCPCondition(prior, next RequiredMCPCondition) RequiredMCPCondition {
	result := prior
	if !next.ObservedAt.Before(prior.ObservedAt) {
		result = next
	}
	availability := prior
	if newerMCPEvidence(next.AvailabilityObservedAt, next.AvailabilityReason, prior.AvailabilityObservedAt, prior.AvailabilityReason) {
		availability = next
	}
	authorization := prior
	if newerMCPEvidence(next.AuthorizationObservedAt, next.AuthorizationReason, prior.AuthorizationObservedAt, prior.AuthorizationReason) {
		authorization = next
	}
	result.AvailabilityObservedAt, result.AvailabilityReason = availability.AvailabilityObservedAt, availability.AvailabilityReason
	result.AuthorizationObservedAt, result.AuthorizationReason = authorization.AuthorizationObservedAt, authorization.AuthorizationReason
	result.Active = result.AvailabilityReason != "" || result.AuthorizationReason != ""
	result.Reason = result.AuthorizationReason
	if result.Reason == "" {
		result.Reason = result.AvailabilityReason
	}
	return result
}

// Availability and authorization have independent evidence clocks. A later
// unobservable check must neither erase a prior denial nor relatch it after a
// different run has supplied newer verified recovery for the same context.
func decisiveMCPObservation(result RequiredMCPCondition) RequiredMCPCondition {
	result.AvailabilityObservedAt, result.AuthorizationObservedAt = time.Time{}, time.Time{}
	result.AvailabilityReason, result.AuthorizationReason = "", ""
	if result.Connection == "ready" && result.Inventory == "ready" {
		result.AvailabilityObservedAt = result.ObservedAt
	}
	switch result.Category {
	case "ready":
		result.AvailabilityObservedAt, result.AuthorizationObservedAt = result.ObservedAt, result.ObservedAt
	case "transport_failure", "required_tool_unavailable":
		result.AvailabilityObservedAt, result.AvailabilityReason = result.ObservedAt, result.Category
	case "authentication_failure", "tool_authorization_failure":
		result.AuthorizationObservedAt, result.AuthorizationReason = result.ObservedAt, result.Category
	}
	return result
}

func validMCPObservationStatus(value string) bool {
	return value == "ready" || value == "unobservable" || value == "denied"
}

// Equal timestamps from different runs cannot establish recovery ordering.
// Retain failure on a tie; the lexical tie-break makes reduction deterministic.
func newerMCPEvidence(next time.Time, nextReason string, prior time.Time, priorReason string) bool {
	return next.After(prior) || next.Equal(prior) && nextReason >= priorReason
}
