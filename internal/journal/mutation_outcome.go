package journal

// IsReferenceTouch includes successful receipt custody copies wherever a
// reader needs external-reference evidence. Failed, conflicting, and
// contended recovery facts remain diagnostics, not successful touches.
func (e Event) IsReferenceTouch() bool {
	if e.ExternalRef == nil {
		return false
	}
	if e.Type == EventRefTouched {
		return true
	}
	if e.Type != EventRunnerMutationRecovered || e.Error != nil {
		return false
	}
	outcome, _ := e.Runner["outcome"].(string)
	return outcome != "failure" && outcome != "conflict" && outcome != "contention"
}

// WithMutationOutcome preserves the enclosing run identity and records the
// claim owner separately: reconciliation may act on another live run's lease.
// Failed attempts are errors, not successful ref touches used by mutation KPIs.
func WithMutationOutcome(event Event, claimRunID, outcome, errorCode, providerRunID string) Event {
	if event.Runner == nil {
		event.Runner = make(map[string]any)
	}
	if claimRunID != "" {
		event.Runner["claimRunId"] = claimRunID
	}
	if outcome != "" {
		event.Runner["outcome"] = outcome
	}
	if providerRunID != "" {
		event.Runner["providerRunId"] = providerRunID
	}
	if outcome == "contention" {
		if event.ExternalRef != nil {
			event.Runner["provider"] = event.ExternalRef.Provider
			event.Runner["kind"] = event.ExternalRef.Kind
			event.Runner["itemId"] = event.ExternalRef.ID
			event.ExternalRef = nil
		}
		event.Type = EventRunnerAnnotation
		event.Reason = "provider claim contention"
		event.Runner["annotation"] = "provider-claim-contention"
		return event
	}
	if outcome == "failure" || outcome == "conflict" {
		event.Type = EventError
		if errorCode == "" {
			errorCode = "provider_claim_" + outcome
		}
		event.Error = &ErrorDetail{Code: errorCode, Message: "provider claim operation did not succeed"}
	}
	return event
}
