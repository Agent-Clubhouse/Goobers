package journal

import (
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// MarshalEvent validates and encodes the shared event envelope without assigning
// sequence/time metadata or scrubbing. Storage backends apply those policies.
func MarshalEvent(event Event) ([]byte, error) { return marshalEvent(event) }

// PrepareOperatorMessageRequest validates and scrubs a request while preserving
// stable, canonical identifiers across retries and redaction.
func PrepareOperatorMessageRequest(scrubber Scrubber, request apiv1.OperatorMessageRequest) (apiv1.OperatorMessageRequest, error) {
	if err := request.Validate(); err != nil {
		return apiv1.OperatorMessageRequest{}, err
	}
	request.RequestID = canonicalOperatorMessageIdentifier(scrubber, "request-id", request.RequestID)
	request.IdempotencyKey = canonicalOperatorMessageIdentifier(scrubber, "idempotency-key", request.IdempotencyKey)
	requestID, idempotencyKey := request.RequestID, request.IdempotencyKey
	request, err := scrubOperatorMessage(scrubber, request)
	if err != nil {
		return apiv1.OperatorMessageRequest{}, err
	}
	request.RequestID, request.IdempotencyKey = requestID, idempotencyKey
	if err := request.Validate(); err != nil {
		return apiv1.OperatorMessageRequest{}, fmt.Errorf("operator message: scrubbed request is invalid: %w", err)
	}
	return request, nil
}

// PrepareOperatorMessageAcknowledgement validates and scrubs the payload while preserving canonical identifiers.
func PrepareOperatorMessageAcknowledgement(scrubber Scrubber, ack apiv1.OperatorMessageAcknowledgement) (apiv1.OperatorMessageAcknowledgement, error) {
	if err := ack.Validate(); err != nil {
		return apiv1.OperatorMessageAcknowledgement{}, err
	}
	ack.RequestID = canonicalOperatorMessageIdentifier(scrubber, "request-id", ack.RequestID)
	ack.IdempotencyKey = canonicalOperatorMessageIdentifier(scrubber, "idempotency-key", ack.IdempotencyKey)
	requestID, idempotencyKey := ack.RequestID, ack.IdempotencyKey
	ack, err := scrubOperatorMessage(scrubber, ack)
	if err != nil {
		return apiv1.OperatorMessageAcknowledgement{}, err
	}
	ack.RequestID, ack.IdempotencyKey = requestID, idempotencyKey
	if err := ack.Validate(); err != nil {
		return apiv1.OperatorMessageAcknowledgement{}, fmt.Errorf("operator message: scrubbed acknowledgement is invalid: %w", err)
	}

	return ack, nil
}

// PrepareOperatorMessageOutcome validates and scrubs the payload while preserving canonical identifiers.
func PrepareOperatorMessageOutcome(scrubber Scrubber, outcome apiv1.OperatorMessageOutcome) (apiv1.OperatorMessageOutcome, error) {
	if err := outcome.Validate(); err != nil {
		return apiv1.OperatorMessageOutcome{}, err
	}
	outcome.RequestID = canonicalOperatorMessageIdentifier(scrubber, "request-id", outcome.RequestID)
	outcome.IdempotencyKey = canonicalOperatorMessageIdentifier(scrubber, "idempotency-key", outcome.IdempotencyKey)
	requestID, idempotencyKey := outcome.RequestID, outcome.IdempotencyKey
	outcome, err := scrubOperatorMessage(scrubber, outcome)
	if err != nil {
		return apiv1.OperatorMessageOutcome{}, err
	}
	outcome.RequestID, outcome.IdempotencyKey = requestID, idempotencyKey
	if err := outcome.Validate(); err != nil {
		return apiv1.OperatorMessageOutcome{}, fmt.Errorf("operator message: scrubbed outcome is invalid: %w", err)
	}

	return outcome, nil
}

// RecoverState rebuilds derived recovery fields from committed events. The
// prior workflow cursor is retained except for a terminal phase or an explicit
// resume not yet reflected by that checkpoint, matching file recovery.
func RecoverState(prior State, events []Event) State {
	state := prior
	state.Schema = StateSchema
	state.Phase = reconstructPhase(events)
	state.Reason = reconstructReason(events)
	state.Branches, _ = reconstructBranchCursors(events)
	state.LastSeq = highestEventSeq(events)
	if len(events) > 0 {
		state.UpdatedAt = events[len(events)-1].Time
	}
	if state.Phase != PhaseRunning {
		state.MachineState = ""
	} else if resumed, ok := latestActiveResume(events); ok && prior.LastSeq <= resumed.Seq {
		state.MachineState = resumed.Target
	}
	return state
}

// CurrentSchemaInfo returns the shared metadata for newly created journals.
func CurrentSchemaInfo() SchemaInfo {
	return SchemaInfo{Version: CurrentSchemaVersion, MinimumBinary: minimumBinaryForJournalSchema(CurrentSchemaVersion)}
}

// ValidateAgentProgressRate applies the same history-dependent append policy
// used by the file writer. Backends call it under their append lock.
func ValidateAgentProgressRate(events []Event, progress AgentProgress, observedAt time.Time) error {
	return validateAgentProgressRate(events, progress, observedAt)
}
