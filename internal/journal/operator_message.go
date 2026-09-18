package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

var (
	ErrOperatorMessageNotFound = errors.New("operator message request not found")
	ErrOperatorMessageTerminal = errors.New("operator message request is terminal")
)

const scrubbedOperatorMessageIdentifierPrefix = "scrubbed:sha256:"

// AcceptOperatorMessage durably accepts a request, atomically resolving an
// existing idempotency key to its original record. Expired requests are not
// accepted; they receive a typed terminal record containing the rejected input.
func (r *Run) AcceptOperatorMessage(request apiv1.OperatorMessageRequest) (apiv1.OperatorMessageRecord, bool, error) {
	if err := request.Validate(); err != nil {
		return apiv1.OperatorMessageRecord{}, false, err
	}
	request.RequestID = canonicalOperatorMessageIdentifier(r.scrubber, "request-id", request.RequestID)
	request.IdempotencyKey = canonicalOperatorMessageIdentifier(r.scrubber, "idempotency-key", request.IdempotencyKey)
	requestID, idempotencyKey := request.RequestID, request.IdempotencyKey
	request, err := scrubOperatorMessage(r.scrubber, request)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, false, err
	}
	request.RequestID, request.IdempotencyKey = requestID, idempotencyKey
	if err := request.Validate(); err != nil {
		return apiv1.OperatorMessageRecord{}, false, fmt.Errorf("operator message: scrubbed request is invalid: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return apiv1.OperatorMessageRecord{}, false, ErrClosed
	}
	records, err := r.operatorMessagesLocked()
	if err != nil {
		return apiv1.OperatorMessageRecord{}, false, err
	}
	for _, record := range records {
		if record.Request.IdempotencyKey == request.IdempotencyKey {
			return record, false, nil
		}
	}

	now := r.now()
	if request.ExpiresAt != nil && !request.ExpiresAt.After(now) {
		outcome := apiv1.OperatorMessageOutcome{
			Schema:         apiv1.OperatorMessageOutcomeSchema,
			RequestID:      request.RequestID,
			IdempotencyKey: request.IdempotencyKey,
			CompletedAt:    now,
			Status:         apiv1.OperatorMessageExpired,
			Code:           "request_expired",
			Request:        &request,
		}
		if err := r.appendOperatorMessageLocked(Event{
			Type:                   EventOperatorMessageOutcome,
			OperatorMessageOutcome: &outcome,
		}); err != nil {
			return apiv1.OperatorMessageRecord{}, false, err
		}
		return apiv1.OperatorMessageRecord{
			Request: request, State: apiv1.OperatorMessageState(outcome.Status), Outcome: &outcome,
		}, false, nil
	}

	if err := r.appendOperatorMessageLocked(Event{
		Type:                   EventOperatorMessageRequested,
		OperatorMessageRequest: &request,
	}); err != nil {
		return apiv1.OperatorMessageRecord{}, false, err
	}
	return apiv1.OperatorMessageRecord{Request: request, State: apiv1.OperatorMessageAccepted}, true, nil
}

// AcknowledgeOperatorMessage appends at most one acknowledgement for a request.
func (r *Run) AcknowledgeOperatorMessage(ack apiv1.OperatorMessageAcknowledgement) (apiv1.OperatorMessageRecord, error) {
	if err := ack.Validate(); err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	ack.RequestID = canonicalOperatorMessageIdentifier(r.scrubber, "request-id", ack.RequestID)
	ack.IdempotencyKey = canonicalOperatorMessageIdentifier(r.scrubber, "idempotency-key", ack.IdempotencyKey)
	requestID, idempotencyKey := ack.RequestID, ack.IdempotencyKey
	ack, err := scrubOperatorMessage(r.scrubber, ack)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	ack.RequestID, ack.IdempotencyKey = requestID, idempotencyKey
	if err := ack.Validate(); err != nil {
		return apiv1.OperatorMessageRecord{}, fmt.Errorf("operator message: scrubbed acknowledgement is invalid: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return apiv1.OperatorMessageRecord{}, ErrClosed
	}
	record, err := r.operatorMessageLocked(ack.IdempotencyKey, ack.RequestID)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	if record.Outcome != nil {
		return apiv1.OperatorMessageRecord{}, ErrOperatorMessageTerminal
	}
	if record.Acknowledgement != nil {
		return record, nil
	}
	if err := r.appendOperatorMessageLocked(Event{
		Type:                           EventOperatorMessageAcknowledged,
		OperatorMessageAcknowledgement: &ack,
	}); err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	record.State = apiv1.OperatorMessageAcknowledged
	record.Acknowledgement = &ack
	return record, nil
}

// CompleteOperatorMessage records the request's terminal delivery disposition.
func (r *Run) CompleteOperatorMessage(outcome apiv1.OperatorMessageOutcome) (apiv1.OperatorMessageRecord, error) {
	if err := outcome.Validate(); err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	if outcome.Request != nil {
		return apiv1.OperatorMessageRecord{}, errors.New("operator message: accepted-request outcome must not embed a request")
	}
	outcome.RequestID = canonicalOperatorMessageIdentifier(r.scrubber, "request-id", outcome.RequestID)
	outcome.IdempotencyKey = canonicalOperatorMessageIdentifier(r.scrubber, "idempotency-key", outcome.IdempotencyKey)
	requestID, idempotencyKey := outcome.RequestID, outcome.IdempotencyKey
	outcome, err := scrubOperatorMessage(r.scrubber, outcome)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	outcome.RequestID, outcome.IdempotencyKey = requestID, idempotencyKey
	if err := outcome.Validate(); err != nil {
		return apiv1.OperatorMessageRecord{}, fmt.Errorf("operator message: scrubbed outcome is invalid: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return apiv1.OperatorMessageRecord{}, ErrClosed
	}
	record, err := r.operatorMessageLocked(outcome.IdempotencyKey, outcome.RequestID)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	if record.Outcome != nil {
		return record, nil
	}
	if err := r.appendOperatorMessageLocked(Event{
		Type:                   EventOperatorMessageOutcome,
		OperatorMessageOutcome: &outcome,
	}); err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	record.State = apiv1.OperatorMessageState(outcome.Status)
	record.Outcome = &outcome
	return record, nil
}

func (r *Run) operatorMessageLocked(idempotencyKey, requestID string) (apiv1.OperatorMessageRecord, error) {
	records, err := r.operatorMessagesLocked()
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	for _, record := range records {
		if record.Request.IdempotencyKey == idempotencyKey {
			if record.Request.RequestID != requestID {
				return apiv1.OperatorMessageRecord{}, fmt.Errorf(
					"operator message: idempotency key belongs to request %q", record.Request.RequestID)
			}
			return record, nil
		}
	}
	return apiv1.OperatorMessageRecord{}, ErrOperatorMessageNotFound
}

func scrubOperatorMessage[T any](scrubber Scrubber, value T) (T, error) {
	var scrubbed T
	raw, err := json.Marshal(value)
	if err != nil {
		return scrubbed, fmt.Errorf("operator message: encode for scrubbing: %w", err)
	}
	if err := json.Unmarshal(scrubber.Scrub(raw), &scrubbed); err != nil {
		return scrubbed, fmt.Errorf("operator message: decode scrubbed payload: %w", err)
	}
	return scrubbed, nil
}

func canonicalOperatorMessageIdentifier(scrubber Scrubber, kind, value string) string {
	if isCanonicalOperatorMessageIdentifier(kind, value) ||
		(!strings.HasPrefix(value, scrubbedOperatorMessageIdentifierPrefix) &&
			bytes.Equal(scrubber.Scrub([]byte(value)), []byte(value))) {
		return value
	}
	sum := sha256.Sum256([]byte("goobers.dev/operator-message/" + kind + "/v1\x00" + value))
	return fmt.Sprintf("%s%s:%x", scrubbedOperatorMessageIdentifierPrefix, kind, sum)
}

func isCanonicalOperatorMessageIdentifier(kind, value string) bool {
	digest, ok := strings.CutPrefix(value, scrubbedOperatorMessageIdentifierPrefix+kind+":")
	if !ok || len(digest) != sha256.Size*2 {
		return false
	}
	for _, char := range digest {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func (r *Run) operatorMessagesLocked() ([]apiv1.OperatorMessageRecord, error) {
	events, _, err := readEvents(filepath.Join(r.dir, fileEvents))
	if err != nil {
		return nil, err
	}
	return ReplayOperatorMessages(events), nil
}

func (r *Run) appendOperatorMessageLocked(event Event) error {
	if err := r.append(event); err != nil {
		return err
	}
	if err := r.checkpoint(); err != nil {
		return err
	}
	if r.observer != nil {
		r.observer(r.id.RunID, r.seq)
	}
	return nil
}

// OperatorMessages reconstructs operator requests from durable journal events.
func (r *Reader) OperatorMessages() ([]apiv1.OperatorMessageRecord, error) {
	events, err := r.Events()
	if err != nil {
		return nil, err
	}
	return ReplayOperatorMessages(events), nil
}

// ReplayOperatorMessages projects accepted and pre-acceptance rejected requests
// in first-seen order. Unknown or orphaned lifecycle records are ignored so an
// older reader remains compatible with repaired or future journals.
func ReplayOperatorMessages(events []Event) []apiv1.OperatorMessageRecord {
	records := make([]apiv1.OperatorMessageRecord, 0)
	byKey := make(map[string]int)
	for _, event := range events {
		switch event.Type {
		case EventOperatorMessageRequested:
			if event.OperatorMessageRequest == nil {
				continue
			}
			request := *event.OperatorMessageRequest
			if _, exists := byKey[request.IdempotencyKey]; exists {
				continue
			}
			byKey[request.IdempotencyKey] = len(records)
			records = append(records, apiv1.OperatorMessageRecord{
				Request: request,
				State:   apiv1.OperatorMessageAccepted,
			})
		case EventOperatorMessageAcknowledged:
			if event.OperatorMessageAcknowledgement == nil {
				continue
			}
			ack := *event.OperatorMessageAcknowledgement
			index, exists := byKey[ack.IdempotencyKey]
			if !exists || records[index].Request.RequestID != ack.RequestID || records[index].Outcome != nil {
				continue
			}
			records[index].State = apiv1.OperatorMessageAcknowledged
			records[index].Acknowledgement = &ack
		case EventOperatorMessageOutcome:
			if event.OperatorMessageOutcome == nil {
				continue
			}
			outcome := *event.OperatorMessageOutcome
			index, exists := byKey[outcome.IdempotencyKey]
			if !exists && outcome.Request != nil {
				request := *outcome.Request
				byKey[request.IdempotencyKey] = len(records)
				records = append(records, apiv1.OperatorMessageRecord{Request: request})
				index, exists = len(records)-1, true
			}
			if !exists || records[index].Request.RequestID != outcome.RequestID {
				continue
			}
			records[index].State = apiv1.OperatorMessageState(outcome.Status)
			records[index].Outcome = &outcome
		}
	}
	return records
}
