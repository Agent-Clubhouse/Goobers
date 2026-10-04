// Package sessioning defines provider-neutral shared conversation contracts.
// Authentication and execution credentials are supplied by trusted services,
// never by these client request bodies.
package sessioning

import "time"

// Bounds apply before durable acceptance and independently of browser lifetime.
const (
	StartKind           = "interactive-session-turn"
	MaxTextBytes        = 64 << 10
	MaxTitleBytes       = 256
	MaxOpenSessions     = 32
	MaxExecutingTurns   = 4
	MaxQueuedTurns      = 64
	MaxRetainedRecords  = 10000
	MaxPageSize         = 200
	MaxMessagePageBytes = 1 << 20
	MaxContextBytes     = 256 << 10
	MaxContextMessages  = 32
	Retention           = 30 * 24 * time.Hour
)

// Actor preserves both parts of the verified human identity without combining
// them into an ambiguous display string.
type Actor struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// Profile pins an existing configured Goober and the archive used to execute it.
type Profile struct {
	Goober           string `json:"goober"`
	ConfigGeneration string `json:"configGeneration"`
	GooberDigest     string `json:"gooberDigest"`
}

// CreateRequest opens an idle shared session. Messages are accepted separately.
type CreateRequest struct {
	RequestID string `json:"requestId"`
	Title     string `json:"title"`
	Goober    string `json:"goober"`
}

// MessageRequest queues ordinary input without interrupting an active turn.
type MessageRequest struct {
	RequestID    string          `json:"requestId"`
	Text         string          `json:"text"`
	RepairTarget *PRRepairTarget `json:"repairTarget,omitempty"`
}

// CloseRequest stops intake and requests cancellation; it is not a stop receipt.
type CloseRequest struct {
	RequestID string `json:"requestId"`
	Reason    string `json:"reason"`
}

// SessionState describes custody. LastOutcome independently describes the last
// settled turn so an idle conversation is never mistaken for a successful run.
type SessionState string

// Session states track durable admission and execution ownership.
const (
	Idle            SessionState = "idle"
	Queued          SessionState = "queued"
	Running         SessionState = "running"
	CancelRequested SessionState = "cancel-requested"
	Closed          SessionState = "closed"
)

// Session is a bounded shared conversation summary.
type Session struct {
	ID     string `json:"id"`
	Gaggle string `json:"gaggle"`
	Title  string `json:"title"`
	Profile
	State        SessionState `json:"state"`
	CreatedBy    Actor        `json:"createdBy"`
	CreatedAt    time.Time    `json:"createdAt"`
	UpdatedAt    time.Time    `json:"updatedAt"`
	NextSequence uint64       `json:"nextSequence"`
	ActiveTurnID string       `json:"activeTurnId,omitempty"`
	LastOutcome  string       `json:"lastOutcome,omitempty"`
}

// Message is append-only content. Agent responses name their execution and
// never inherit the originating human's authorship.
type Message struct {
	ID           string          `json:"id"`
	SessionID    string          `json:"sessionId"`
	Sequence     uint64          `json:"sequence"`
	ActorKind    string          `json:"actorKind"`
	Actor        *Actor          `json:"actor,omitempty"`
	Text         string          `json:"text"`
	CreatedAt    time.Time       `json:"createdAt"`
	TurnID       string          `json:"turnId,omitempty"`
	RunID        string          `json:"runId,omitempty"`
	Outcome      string          `json:"outcome,omitempty"`
	RepairTarget *PRRepairTarget `json:"repairTarget,omitempty"`
}

// Acceptance confirms only durable command custody, not model execution.
type Acceptance struct {
	Session      Session  `json:"session"`
	Message      *Message `json:"message,omitempty"`
	AcceptanceID string   `json:"acceptanceId,omitempty"`
	Duplicate    bool     `json:"duplicate"`
}

// SessionPage uses stable opaque session IDs as pagination cursors.
type SessionPage struct {
	Items      []Session `json:"items"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

// MessagePage uses append-only numeric message sequences as cursors.
type MessagePage struct {
	Items      []Message `json:"items"`
	NextCursor uint64    `json:"nextCursor,omitempty"`
}

// StartEnvelope is host-created queue provenance. Message bodies and human
// authorization snapshots remain in the bounded session ledger.
type StartEnvelope struct {
	Kind            string `json:"kind"`
	Gaggle          string `json:"gaggle"`
	SessionID       string `json:"sessionId"`
	TurnID          string `json:"turnId"`
	MessageID       string `json:"messageId"`
	MessageDigest   string `json:"messageDigest"`
	AuthorityDigest string `json:"authorityDigest"`
	Profile
}
