package apicontract

import (
	"time"

	"github.com/goobers/goobers/internal/sessioning"
)

// SessionCreateRequest carries content only. Idempotency keys and authenticated
// principals are supplied separately by the HTTP transport.
type SessionCreateRequest struct {
	Title  string `json:"title"`
	Goober string `json:"goober"`
}

// SessionMessageRequest queues human content without authority fields.
// SessionMessage is an attributed append-only message.
type SessionMessageRequest struct {
	Text         string                     `json:"text"`
	RepairTarget *sessioning.PRRepairTarget `json:"repairTarget,omitempty"`
}

// SessionCloseRequest requests intake closure and cancellation.
type SessionCloseRequest struct {
	Reason string `json:"reason"`
}

// InteractiveSession is the shared conversation summary.
type InteractiveSession = sessioning.Session

// SessionMessage is an attributed append-only message.
type SessionMessage = sessioning.Message

// SessionAcceptance acknowledges durable command custody.
type SessionAcceptance = sessioning.Acceptance

// SessionPage contains bounded session summaries.
type SessionPage = sessioning.SessionPage

// SessionMessagePage contains a bounded ordered message page.
type SessionMessagePage = sessioning.MessagePage

func sessionRoute(id RouteID) bool {
	switch id {
	case RouteSessionList, RouteSessionCreate, RouteSessionGet, RouteSessionMessages, RouteSessionMessage, RouteSessionClose:
		return true
	}
	return false
}

func withSessionFixtures(fixtures wireFixtures) wireFixtures {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	fixtures.SessionCreate = SessionCreateRequest{Title: "Scope the next feature", Goober: "planner"}
	fixtures.SessionInput = SessionMessageRequest{Text: "Outline the work and open questions."}
	fixtures.SessionClose = SessionCloseRequest{Reason: "Scoping complete."}
	fixtures.Session = InteractiveSession{ID: "session-one", Gaggle: "web", Title: fixtures.SessionCreate.Title, Profile: sessioning.Profile{Goober: "planner", ConfigGeneration: "generation-one", GooberDigest: "digest-one"}, State: sessioning.Queued, CreatedBy: sessioning.Actor{Issuer: "https://identity.example", Subject: "alice"}, CreatedAt: at, UpdatedAt: at, NextSequence: 2}
	message := SessionMessage{ID: "message-one", SessionID: "session-one", Sequence: 1, ActorKind: "human", Actor: &fixtures.Session.CreatedBy, Text: fixtures.SessionInput.Text, CreatedAt: at, TurnID: "turn-one"}
	fixtures.Sessions = SessionPage{Items: []InteractiveSession{fixtures.Session}}
	fixtures.SessionMessages = SessionMessagePage{Items: []SessionMessage{message}}
	fixtures.SessionAccepted = SessionAcceptance{Session: fixtures.Session, Message: &message, AcceptanceID: "accepted-one"}
	return fixtures
}
