package apicontract

import "time"

// TriggerRequest submits a workflow trigger. Authority fields are transport-
// stamped and excluded from JSON; callers supply only the trigger parameters.
type TriggerRequest struct {
	DispatchRunID string `json:"-"`
	Actor         string `json:"-"`
	PodScoped     bool   `json:"-"`
	PodRunID      string `json:"-"`
	Gaggle        string `json:"gaggle,omitempty"`
	Workflow      string `json:"workflow"`
	RequestID     string `json:"requestId,omitempty"`
	// Force bypasses only hourly/daily cadence budgets; it is invalid for
	// priority and pod-scoped triggers.
	Force bool `json:"force,omitempty"`
	// SourceRun requests a priority re-tick following published durable state.
	// Readiness still applies; a pod principal must name its own run and gaggle.
	SourceRun string `json:"sourceRun,omitempty"`
}

// TriggerResponse acknowledges acceptance independently of run dispatch.
type TriggerResponse struct {
	AcceptanceID string `json:"acceptanceId,omitempty"`
	State        string `json:"state,omitempty"`
	RunID        string `json:"runId,omitempty"`
	Duplicate    bool   `json:"duplicate,omitempty"`
}

// TriggerStatusResponse reports the durable acceptance ledger's current state.
type TriggerStatusResponse struct {
	AcceptanceID string    `json:"acceptanceId"`
	State        string    `json:"state"`
	RunID        string    `json:"runId,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	AcceptedAt   time.Time `json:"acceptedAt"`
}

// CancelRunRequest names optional identity constraints. The route stamps RunID,
// IdempotencyKey, and (when authenticated) Actor before invoking the service.
type CancelRunRequest struct {
	IdempotencyKey string `json:"-"`
	RunID          string `json:"-"`
	Workflow       string `json:"workflow,omitempty"`
	Gaggle         string `json:"gaggle,omitempty"`
	Actor          string `json:"actor,omitempty"`
}

// CancelRunResult distinguishes requested engine cancellation from termination.
type CancelRunResult struct {
	Phase string `json:"phase,omitempty"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}
