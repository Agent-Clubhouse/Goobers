package apicontract

import "time"

// GaggleEventPublishPath and related IDs name the scoped external event API.
const (
	GaggleEventPublishPath          = V1Prefix + "/gaggles/{gaggle}/events"
	GaggleEventReceiptPath          = GaggleEventPublishPath + "/{receipt}"
	RouteGaggleEventPublish RouteID = "gaggleEventPublish"
	RouteGaggleEventReceipt RouteID = "gaggleEventReceipt"
	EventBindingHeader              = "X-Goobers-Event-Binding"
)

// GaggleEventEnvelope is the closed JSON CloudEvents ingress profile. Payload
// values carry no authority, even when named after host provenance fields.
type GaggleEventEnvelope struct {
	SpecVersion     string `json:"specversion"`
	ID              string `json:"id"`
	Source          string `json:"source"`
	Type            string `json:"type"`
	Subject         string `json:"subject,omitempty"`
	Time            string `json:"time,omitempty"`
	DataSchema      string `json:"dataschema,omitempty"`
	DataContentType string `json:"datacontenttype,omitempty"`
	Data            any    `json:"data,omitempty"`
}

// GaggleEventReceipt reports durable acceptance, not consumer completion.
type GaggleEventReceipt struct {
	ReceiptID  string                `json:"receiptId"`
	Gaggle     string                `json:"gaggle"`
	Binding    string                `json:"binding"`
	Source     string                `json:"source"`
	EventID    string                `json:"eventId"`
	Digest     string                `json:"digest"`
	AcceptedAt time.Time             `json:"acceptedAt"`
	Duplicate  bool                  `json:"duplicate"`
	State      string                `json:"state"`
	StatusURL  string                `json:"statusUrl"`
	Tombstoned bool                  `json:"tombstoned"`
	Deliveries []GaggleEventDelivery `json:"deliveries"`
}

// GaggleEventDelivery links one consumer's routing result to its accepted start.
type GaggleEventDelivery struct {
	Consumer     string `json:"consumer"`
	GroupID      string `json:"groupId,omitempty"`
	Reason       string `json:"reason,omitempty"`
	State        string `json:"state,omitempty"`
	AcceptanceID string `json:"acceptanceId,omitempty"`
	RunID        string `json:"runId,omitempty"`
}

func eventIngressRoute(id RouteID) bool {
	return id == RouteGaggleEventPublish || id == RouteGaggleEventReceipt
}

func withEventIngressFixtures(f wireFixtures) wireFixtures {
	f.GaggleEventEnvelope = GaggleEventEnvelope{SpecVersion: "1.0", ID: "change-1", Source: "urn:factory:builds", Type: "build.finished", Data: map[string]any{"result": "passed"}}
	f.GaggleEventReceipt = GaggleEventReceipt{ReceiptID: "event-0123456789abcdef0123456789abcdef", Gaggle: "web", Binding: "ingress:builds", Source: f.GaggleEventEnvelope.Source, EventID: f.GaggleEventEnvelope.ID, Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", AcceptedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), State: "routing_pending", StatusURL: "/api/v1/gaggles/web/events/event-0123456789abcdef0123456789abcdef", Deliveries: []GaggleEventDelivery{}}
	return f
}
