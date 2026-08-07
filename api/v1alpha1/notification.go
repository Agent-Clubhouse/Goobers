package v1alpha1

import "time"

const (
	// NotificationRequestSchema identifies the provider-neutral notification request contract.
	NotificationRequestSchema = "goobers.dev/notification/request/v1"
	// NotificationReceiptSchema identifies the durable sink delivery receipt contract.
	NotificationReceiptSchema = "goobers.dev/notification/receipt/v1"
)

// NotificationSeverity is the operational importance of a notification.
type NotificationSeverity string

// Supported notification severities.
const (
	NotificationSeverityInfo     NotificationSeverity = "info"
	NotificationSeverityWarning  NotificationSeverity = "warning"
	NotificationSeverityError    NotificationSeverity = "error"
	NotificationSeverityCritical NotificationSeverity = "critical"
)

// NotificationSource identifies the workflow location that produced a request.
type NotificationSource struct {
	RunID    string `json:"runId"`
	Workflow string `json:"workflow"`
	Stage    string `json:"stage"`
}

// NotificationFact is one deterministic name/value fact rendered before dispatch.
type NotificationFact struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// NotificationEvidenceRef links a request to supporting evidence without
// introducing transport-provider fields into the request.
type NotificationEvidenceRef struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Digest string `json:"digest,omitempty"`
}

// NotificationRequest is exact, pre-rendered content for one or more sinks.
type NotificationRequest struct {
	Schema         string                    `json:"schema"`
	NotificationID string                    `json:"notificationId"`
	IncidentID     string                    `json:"incidentId"`
	EventID        string                    `json:"eventId"`
	Severity       NotificationSeverity      `json:"severity"`
	Transition     string                    `json:"transition"`
	Title          string                    `json:"title"`
	Body           string                    `json:"body"`
	SpeechText     string                    `json:"speechText,omitempty"`
	Facts          []NotificationFact        `json:"facts,omitempty"`
	Evidence       []NotificationEvidenceRef `json:"evidence,omitempty"`
	Source         NotificationSource        `json:"source"`
	Sinks          []string                  `json:"sinks"`
	ExpiresAt      time.Time                 `json:"expiresAt"`
	IdempotencyKey string                    `json:"idempotencyKey"`
}

// NotificationDeliveryStatus is the outcome of one sink attempt or suppression.
type NotificationDeliveryStatus string

// Notification delivery states recorded by sinks.
const (
	NotificationPending   NotificationDeliveryStatus = "pending"
	NotificationDelivered NotificationDeliveryStatus = "delivered"
	NotificationFailed    NotificationDeliveryStatus = "failed"
	NotificationSkipped   NotificationDeliveryStatus = "skipped"
)

// NotificationSinkRef identifies a registered sink implementation.
type NotificationSinkRef struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

// NotificationReceipt durably records one sink attempt or suppression.
type NotificationReceipt struct {
	Schema            string                     `json:"schema"`
	NotificationID    string                     `json:"notificationId"`
	IdempotencyKey    string                     `json:"idempotencyKey"`
	IdempotencyDigest string                     `json:"idempotencyDigest"`
	Source            NotificationSource         `json:"source"`
	Evidence          []NotificationEvidenceRef  `json:"evidence,omitempty"`
	Sink              NotificationSinkRef        `json:"sink"`
	Attempt           int                        `json:"attempt"`
	StartedAt         time.Time                  `json:"startedAt"`
	CompletedAt       time.Time                  `json:"completedAt"`
	Status            NotificationDeliveryStatus `json:"status"`
	Unresolved        bool                       `json:"unresolved,omitempty"`
	ExternalReference string                     `json:"externalReference,omitempty"`
	Error             string                     `json:"error,omitempty"`
}
