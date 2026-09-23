package v1alpha1

import (
	"strings"
	"testing"
	"time"
)

func TestOperatorMessageValidationMatchesSchemaBounds(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	request := OperatorMessageRequest{
		Schema:         OperatorMessageRequestSchema,
		RequestID:      "request",
		IdempotencyKey: "key",
		TargetAddress:  "terminal:operator",
		PrincipalRef:   "user:requester",
		RequestedAt:    now,
		Purpose:        "approval",
		Content:        OperatorMessageContent{Text: "review"},
		DeliveryMode:   "terminal",
	}
	ack := OperatorMessageAcknowledgement{
		Schema: OperatorMessageAcknowledgementSchema, RequestID: "request",
		IdempotencyKey: "key", PrincipalRef: "user:operator", AcknowledgedAt: now,
	}
	outcome := OperatorMessageOutcome{
		Schema: OperatorMessageOutcomeSchema, RequestID: "request",
		IdempotencyKey: "key", CompletedAt: now, Status: OperatorMessageDelivered,
	}

	tests := []struct {
		name     string
		validate func() error
	}{
		{"request id", func() error {
			value := request
			value.RequestID = strings.Repeat("x", MaxOperatorMessageRequestIDRunes+1)
			return value.Validate()
		}},
		{"idempotency key", func() error {
			value := request
			value.IdempotencyKey = strings.Repeat("x", MaxOperatorMessageIdempotencyKeyRunes+1)
			return value.Validate()
		}},
		{"target address", func() error {
			value := request
			value.TargetAddress = strings.Repeat("x", MaxOperatorMessageTargetAddressRunes+1)
			return value.Validate()
		}},
		{"request principal", func() error {
			value := request
			value.PrincipalRef = strings.Repeat("x", MaxOperatorMessagePrincipalRefRunes+1)
			return value.Validate()
		}},
		{"purpose", func() error {
			value := request
			value.Purpose = strings.Repeat("x", MaxOperatorMessagePurposeRunes+1)
			return value.Validate()
		}},
		{"delivery mode", func() error {
			value := request
			value.DeliveryMode = strings.Repeat("x", MaxOperatorMessageDeliveryModeRunes+1)
			return value.Validate()
		}},
		{"acknowledgement request id", func() error {
			value := ack
			value.RequestID = strings.Repeat("x", MaxOperatorMessageRequestIDRunes+1)
			return value.Validate()
		}},
		{"acknowledgement key", func() error {
			value := ack
			value.IdempotencyKey = strings.Repeat("x", MaxOperatorMessageIdempotencyKeyRunes+1)
			return value.Validate()
		}},
		{"acknowledgement principal", func() error {
			value := ack
			value.PrincipalRef = strings.Repeat("x", MaxOperatorMessagePrincipalRefRunes+1)
			return value.Validate()
		}},
		{"outcome request id", func() error {
			value := outcome
			value.RequestID = strings.Repeat("x", MaxOperatorMessageRequestIDRunes+1)
			return value.Validate()
		}},
		{"outcome key", func() error {
			value := outcome
			value.IdempotencyKey = strings.Repeat("x", MaxOperatorMessageIdempotencyKeyRunes+1)
			return value.Validate()
		}},
		{"outcome code", func() error {
			value := outcome
			value.Code = strings.Repeat("x", MaxOperatorMessageOutcomeCodeRunes+1)
			return value.Validate()
		}},
		{"outcome detail", func() error {
			value := outcome
			value.Detail = strings.Repeat("x", MaxOperatorMessageOutcomeDetailRunes+1)
			return value.Validate()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.validate(); err == nil {
				t.Fatal("Validate accepted an overlong field")
			}
		})
	}

	request.RequestID = strings.Repeat("界", MaxOperatorMessageRequestIDRunes)
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate rejected a request at the schema character limit: %v", err)
	}
	request.Content.Text = strings.Repeat("界", MaxOperatorMessageContentRunes)
	if err := request.Validate(); err != nil {
		t.Fatalf("Validate rejected multibyte content at the schema character limit: %v", err)
	}
	request.Content.Text += "界"
	if err := request.Validate(); err == nil {
		t.Fatal("Validate accepted multibyte content above the schema character limit")
	}
	if err := ack.Validate(); err != nil {
		t.Fatalf("Validate rejected valid acknowledgement: %v", err)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatalf("Validate rejected valid outcome: %v", err)
	}
}
