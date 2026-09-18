package v1alpha1

import (
	"errors"
	"fmt"
	"time"
)

const (
	OperatorMessageRequestSchema         = "goobers.dev/operator-message/request/v1"
	OperatorMessageAcknowledgementSchema = "goobers.dev/operator-message/acknowledgement/v1"
	OperatorMessageOutcomeSchema         = "goobers.dev/operator-message/outcome/v1"

	MaxOperatorMessageContentBytes int64 = 1 << 20
)

// OperatorMessageContent contains either scrubbed inline text or a bounded
// reference to content already retained by the run journal.
type OperatorMessageContent struct {
	Text     string           `json:"text,omitempty"`
	Artifact *ArtifactPointer `json:"artifact,omitempty"`
}

// OperatorMessageRequest is the durable, provider-neutral operator message.
type OperatorMessageRequest struct {
	Schema         string                 `json:"schema"`
	RequestID      string                 `json:"requestId"`
	IdempotencyKey string                 `json:"idempotencyKey"`
	TargetAddress  string                 `json:"targetAddress"`
	PrincipalRef   string                 `json:"principalRef"`
	RequestedAt    time.Time              `json:"requestedAt"`
	ExpiresAt      *time.Time             `json:"expiresAt,omitempty"`
	Purpose        string                 `json:"purpose"`
	Content        OperatorMessageContent `json:"content"`
	DeliveryMode   string                 `json:"deliveryMode"`
}

// Validate enforces the request invariants shared by every producer.
func (r OperatorMessageRequest) Validate() error {
	if r.Schema != OperatorMessageRequestSchema {
		return fmt.Errorf("operator message: unsupported request schema %q", r.Schema)
	}
	if r.RequestID == "" || r.IdempotencyKey == "" || r.TargetAddress == "" ||
		r.PrincipalRef == "" || r.RequestedAt.IsZero() || r.Purpose == "" || r.DeliveryMode == "" {
		return errors.New("operator message: request identity, target, principal, timestamp, purpose, and delivery mode are required")
	}
	hasText := r.Content.Text != ""
	hasArtifact := r.Content.Artifact != nil
	if hasText == hasArtifact {
		return errors.New("operator message: content must contain exactly one of text or artifact")
	}
	if int64(len(r.Content.Text)) > MaxOperatorMessageContentBytes {
		return errors.New("operator message: inline content exceeds size limit")
	}
	if hasArtifact {
		if err := r.Content.Artifact.Validate(); err != nil {
			return fmt.Errorf("operator message: invalid content artifact: %w", err)
		}
		if r.Content.Artifact.Size <= 0 || r.Content.Artifact.Size > MaxOperatorMessageContentBytes {
			return errors.New("operator message: content artifact exceeds size limit")
		}
	}
	return nil
}

// OperatorMessageAcknowledgement records an operator's durable acknowledgement.
type OperatorMessageAcknowledgement struct {
	Schema         string    `json:"schema"`
	RequestID      string    `json:"requestId"`
	IdempotencyKey string    `json:"idempotencyKey"`
	PrincipalRef   string    `json:"principalRef"`
	AcknowledgedAt time.Time `json:"acknowledgedAt"`
}

// OperatorMessageOutcomeStatus is a terminal delivery disposition.
type OperatorMessageOutcomeStatus string

const (
	OperatorMessageDelivered OperatorMessageOutcomeStatus = "delivered"
	OperatorMessageFailed    OperatorMessageOutcomeStatus = "failed"
	OperatorMessageRejected  OperatorMessageOutcomeStatus = "rejected"
	OperatorMessageExpired   OperatorMessageOutcomeStatus = "expired"
)

// OperatorMessageOutcome records a terminal request disposition. Request is
// populated when a request is rejected before acceptance (for example expiry),
// preserving enough input to reconstruct that terminal record.
type OperatorMessageOutcome struct {
	Schema         string                       `json:"schema"`
	RequestID      string                       `json:"requestId"`
	IdempotencyKey string                       `json:"idempotencyKey"`
	CompletedAt    time.Time                    `json:"completedAt"`
	Status         OperatorMessageOutcomeStatus `json:"status"`
	Code           string                       `json:"code,omitempty"`
	Detail         string                       `json:"detail,omitempty"`
	Request        *OperatorMessageRequest      `json:"request,omitempty"`
}

// OperatorMessageState is the replayed lifecycle state.
type OperatorMessageState string

const (
	OperatorMessageAccepted     OperatorMessageState = "accepted"
	OperatorMessageAcknowledged OperatorMessageState = "acknowledged"
)

// OperatorMessageRecord is a replayed request and its latest durable state.
type OperatorMessageRecord struct {
	Request         OperatorMessageRequest          `json:"request"`
	State           OperatorMessageState            `json:"state"`
	Acknowledgement *OperatorMessageAcknowledgement `json:"acknowledgement,omitempty"`
	Outcome         *OperatorMessageOutcome         `json:"outcome,omitempty"`
}
