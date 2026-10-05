package apicontract

import "time"

// StartQueuePath and related routes expose authorized durable start custody.
const (
	StartQueuePath                = V1Prefix + "/gaggles/{gaggle}/start-queue"
	StartQueueItemPath            = StartQueuePath + "/{acceptance}"
	StartQueueCancelPath          = StartQueueItemPath + "/cancel"
	RouteStartQueue       RouteID = "startQueue"
	RouteStartQueueItem   RouteID = "startQueueItem"
	RouteStartQueueCancel RouteID = "startQueueCancel"
)

// StartQueueCancelInput contains intent only. Human identity is authenticated.
type StartQueueCancelInput struct {
	RequestID string `json:"requestId"`
	Reason    string `json:"reason"`
}

// StartQueueCancellation reports receipt custody; requested does not mean stopped.
type StartQueueCancellation struct {
	RequestID   string    `json:"requestId"`
	Actor       string    `json:"actor"`
	Reason      string    `json:"reason"`
	RequestedAt time.Time `json:"requestedAt"`
	State       string    `json:"state"`
}

// StartQueueItem omits payloads, raw authority, credentials and source addresses.
type StartQueueItem struct {
	AcceptanceID  string                  `json:"acceptanceId"`
	Gaggle        string                  `json:"gaggle"`
	Workflow      string                  `json:"workflow"`
	Source        string                  `json:"source"`
	Generation    string                  `json:"generation"`
	AcceptedAt    time.Time               `json:"acceptedAt"`
	Deadline      *time.Time              `json:"deadline,omitempty"`
	State         string                  `json:"state"`
	WaitingReason string                  `json:"waitingReason,omitempty"`
	RunID         string                  `json:"runId,omitempty"`
	Disposition   string                  `json:"disposition,omitempty"`
	Cancellation  *StartQueueCancellation `json:"cancellation,omitempty"`
}

// StartQueuePage is a bounded explicit navigation window; no progress is inferred.
type StartQueuePage struct {
	Gaggle     string           `json:"gaggle"`
	Items      []StartQueueItem `json:"items"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

func startQueueRoute(id RouteID) bool {
	return id == RouteStartQueue || id == RouteStartQueueItem || id == RouteStartQueueCancel
}
func withStartQueueFixtures(f wireFixtures) wireFixtures {
	item := StartQueueItem{AcceptanceID: "trigger-0123456789abcdef0123456789abcdef", Gaggle: "web", Workflow: "repair", Source: "manual", Generation: "sha256:1234", AcceptedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), State: "accepted", WaitingReason: "Waiting for workflow capacity or budget."}
	f.StartQueue = StartQueuePage{Gaggle: "web", Items: []StartQueueItem{item}}
	f.StartQueueItem = item
	f.StartQueueCancel = StartQueueCancelInput{RequestID: "request-1", Reason: "No longer needed"}
	return f
}
