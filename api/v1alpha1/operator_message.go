package v1alpha1

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

const (
	OperatorMessageRequestSchema         = "goobers.dev/operator-message/request/v1"
	OperatorMessageAcknowledgementSchema = "goobers.dev/operator-message/acknowledgement/v1"
	OperatorMessageOutcomeSchema         = "goobers.dev/operator-message/outcome/v1"

	MaxOperatorMessageContentBytes        int64 = 1 << 20
	MaxOperatorMessageRequestIDRunes            = 256
	MaxOperatorMessageIdempotencyKeyRunes       = 256
	MaxOperatorMessageTargetAddressRunes        = 2048
	MaxOperatorMessagePrincipalRefRunes         = 512
	MaxOperatorMessagePurposeRunes              = 256
	MaxOperatorMessageDeliveryModeRunes         = 128
	MaxOperatorMessageOutcomeCodeRunes          = 128
	MaxOperatorMessageOutcomeDetailRunes        = 4096
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
	if exceedsRunes(r.RequestID, MaxOperatorMessageRequestIDRunes) ||
		exceedsRunes(r.IdempotencyKey, MaxOperatorMessageIdempotencyKeyRunes) ||
		exceedsRunes(r.TargetAddress, MaxOperatorMessageTargetAddressRunes) ||
		exceedsRunes(r.PrincipalRef, MaxOperatorMessagePrincipalRefRunes) ||
		exceedsRunes(r.Purpose, MaxOperatorMessagePurposeRunes) ||
		exceedsRunes(r.DeliveryMode, MaxOperatorMessageDeliveryModeRunes) {
		return errors.New("operator message: request metadata exceeds size limit")
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

// Validate enforces the acknowledgement invariants shared by every producer.
func (a OperatorMessageAcknowledgement) Validate() error {
	if a.Schema != OperatorMessageAcknowledgementSchema {
		return fmt.Errorf("operator message: unsupported acknowledgement schema %q", a.Schema)
	}
	if a.RequestID == "" || a.IdempotencyKey == "" || a.PrincipalRef == "" || a.AcknowledgedAt.IsZero() {
		return errors.New("operator message: acknowledgement identity, principal, and timestamp are required")
	}
	if exceedsRunes(a.RequestID, MaxOperatorMessageRequestIDRunes) ||
		exceedsRunes(a.IdempotencyKey, MaxOperatorMessageIdempotencyKeyRunes) ||
		exceedsRunes(a.PrincipalRef, MaxOperatorMessagePrincipalRefRunes) {
		return errors.New("operator message: acknowledgement metadata exceeds size limit")
	}
	return nil
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

// Validate enforces the outcome invariants shared by every producer.
func (o OperatorMessageOutcome) Validate() error {
	if o.Schema != OperatorMessageOutcomeSchema {
		return fmt.Errorf("operator message: unsupported outcome schema %q", o.Schema)
	}
	if o.RequestID == "" || o.IdempotencyKey == "" || o.CompletedAt.IsZero() || !o.Status.valid() {
		return errors.New("operator message: outcome identity, timestamp, and status are required")
	}
	if exceedsRunes(o.RequestID, MaxOperatorMessageRequestIDRunes) ||
		exceedsRunes(o.IdempotencyKey, MaxOperatorMessageIdempotencyKeyRunes) ||
		exceedsRunes(o.Code, MaxOperatorMessageOutcomeCodeRunes) ||
		exceedsRunes(o.Detail, MaxOperatorMessageOutcomeDetailRunes) {
		return errors.New("operator message: outcome metadata exceeds size limit")
	}
	if o.Request != nil {
		if err := o.Request.Validate(); err != nil {
			return fmt.Errorf("operator message: invalid outcome request: %w", err)
		}
	}
	return nil
}

func (s OperatorMessageOutcomeStatus) valid() bool {
	switch s {
	case OperatorMessageDelivered, OperatorMessageFailed, OperatorMessageRejected, OperatorMessageExpired:
		return true
	default:
		return false
	}
}

func exceedsRunes(value string, limit int) bool {
	return utf8.RuneCountInString(value) > limit
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
