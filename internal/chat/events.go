package chat

import (
	"sync"
	"time"
)

// ContractVersion is the major version carried by every normalized event.
const ContractVersion = 2

// Event contains normalized payloads only, never raw RPC params. Fence is
// the host lease fence, not a model-provided identity. Seq orders this stream;
// the durable host store assigns its own global sequence on projection.
type Event struct {
	V         int            `json:"v"`
	AgentID   string         `json:"agentId"`
	SessionID string         `json:"sessionId"`
	TurnID    string         `json:"turnId,omitempty"`
	Seq       uint64         `json:"seq"`
	TS        int64          `json:"ts"`
	Fence     uint64         `json:"fence"`
	CommandID string         `json:"commandId,omitempty"`
	Type      string         `json:"type"`
	Data      map[string]any `json:"data"`
}

var eventTypes = map[string]bool{}

func init() {
	for _, t := range []string{
		"session_started", "session_info", "session_state", "session_ended", "turn_started", "turn_ended",
		"user_message", "agent_message_chunk", "agent_message", "agent_thought_chunk", "agent_thought",
		"tool_call", "tool_call_update", "permission_request", "permission_resolved", "question", "question_resolved",
		"plan", "plan_document", "subagent", "usage", "mode_changed", "model_changed", "commands_available", "workspace_changed",
		"notice", "error", "protocol_unknown", "command.accepted", "command.delivered", "command.rejected",
		"custody.acquired", "custody.released", "custody.pending", "completion.accepted", "completion.rejected",
	} {
		eventTypes[t] = true
	}
}

// KnownEvent reports whether an event belongs to the current contract vocabulary.
func KnownEvent(t string) bool { return eventTypes[t] }

// Stream serializes envelope assignment. Emit never blocks a protocol callback;
// overflow is a fatal transport failure, signalled by false. Owners must stop
// writers on false. Close never waits for a subscriber.
type Stream struct {
	mu      sync.Mutex
	options StartOptions
	seq     uint64
	events  chan Event
	closed  bool
}

// NewStream creates a bounded stream with host-assigned identity and ordering.
func NewStream(o StartOptions) *Stream {
	return &Stream{options: o, seq: o.LastSeq, events: make(chan Event, 1024)}
}

// Events returns the ordered event channel.
func (s *Stream) Events() <-chan Event { return s.events }

// Emit adds a normalized event, returning false if the stream cannot accept it.
func (s *Stream) Emit(t string, data map[string]any, commandID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if !KnownEvent(t) {
		data = map[string]any{"kind": "field", "name": t}
		t = "protocol_unknown"
	}
	if data == nil {
		data = map[string]any{}
	}
	e := Event{V: ContractVersion, AgentID: s.options.AgentID, SessionID: s.options.SessionID, TurnID: s.options.TurnID, Seq: s.seq + 1, TS: time.Now().UnixMilli(), Fence: s.options.Fence, CommandID: commandID, Type: t, Data: data}
	select {
	case s.events <- e:
		s.seq++
		return true
	default:
		return false
	}
}

// Sequence returns the latest published stream boundary.
func (s *Stream) Sequence() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Close closes the session or stream after releasing its owned resources.
func (s *Stream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
	}
}

// UserMessage is shared with the portal projection. Content is always
// normalized blocks; tool input is omitted rather than persisting RPC params.
type UserMessage struct {
	Content  []ContentBlock `json:"content"`
	Source   string         `json:"source"`
	Delivery Delivery       `json:"delivery"`
}

// ToolCall describes a normalized tool invocation without raw RPC arguments.
type ToolCall struct {
	ToolCallID string         `json:"toolCallId"`
	Title      string         `json:"title"`
	Kind       string         `json:"kind"`
	Status     string         `json:"status"`
	Content    []ContentBlock `json:"content,omitempty"`
}

// ToolCallUpdate updates a normalized tool invocation by identity.
type ToolCallUpdate struct {
	ToolCallID string         `json:"toolCallId"`
	Status     string         `json:"status"`
	Content    []ContentBlock `json:"content,omitempty"`
}
