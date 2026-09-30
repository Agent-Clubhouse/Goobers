package apicontract

import (
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// OperatorMessageSubmitRequest carries caller-supplied message data. The
// transport stamps RunID, IdempotencyKey, PrincipalRef, and RequestedAt.
type OperatorMessageSubmitRequest struct {
	RunID          string    `json:"-"`
	IdempotencyKey string    `json:"-"`
	PrincipalRef   string    `json:"-"`
	RequestedAt    time.Time `json:"-"`

	Gaggle        string                       `json:"gaggle"`
	TargetAddress string                       `json:"targetAddress"`
	ExpiresAt     *time.Time                   `json:"expiresAt,omitempty"`
	Purpose       string                       `json:"purpose"`
	Content       apiv1.OperatorMessageContent `json:"content"`
	DeliveryMode  string                       `json:"deliveryMode"`
}

// OperatorMessageSubmitResponse reports the durable journal result.
type OperatorMessageSubmitResponse struct {
	Accepted bool                        `json:"accepted"`
	Record   apiv1.OperatorMessageRecord `json:"record"`
}
