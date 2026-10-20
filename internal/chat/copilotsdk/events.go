package copilotsdk

import (
	sdk "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"

	"github.com/goobers/goobers/internal/chat"
)

// onEvent projects an allowlist of typed fields. Neither raw RPC params nor
// tool arguments enter the normalized stream. Unknown types remain observable.
func (s *session) onEvent(e sdk.SessionEvent) {
	root := e.AgentID == nil || *e.AgentID == ""
	switch d := e.Data.(type) {
	case *rpc.AssistantTurnStartData:
		if !root {
			return
		}
		s.mu.Lock()
		if !s.busy {
			s.interrupted = false
		}
		s.busy = true
		s.mu.Unlock()
		s.emit("turn_started", map[string]any{}, "")
	case *rpc.AssistantTurnEndData:
		// This can precede a follow-up agent turn (#2191). Only session.idle
		// settles the conversational turn and allows completion handling.
	case *rpc.SessionIdleData:
		if !root || d.Mode != nil && *d.Mode == rpc.SessionModeAutopilot {
			return
		}
		s.finishTurn(d.Aborted != nil && *d.Aborted)
	case *rpc.AbortData:
		if root {
			s.finishTurn(true)
		}
	case *rpc.AssistantMessageData:
		s.emit("agent_message", map[string]any{"content": chat.Text(d.Content), "messageId": d.MessageID}, "")
	case *rpc.AssistantMessageDeltaData:
		s.emit("agent_message_chunk", map[string]any{"content": chat.Text(d.DeltaContent), "messageId": d.MessageID}, "")
	case *rpc.ToolExecutionStartData:
		s.emit("tool_call", map[string]any{"toolCallId": d.ToolCallID, "title": d.ToolName, "kind": "other", "status": "in_progress"}, "")
	case *rpc.ToolExecutionCompleteData:
		status := "completed"
		if !d.Success {
			status = "failed"
		}
		s.emit("tool_call_update", map[string]any{"toolCallId": d.ToolCallID, "status": status}, "")
	case *rpc.ToolExecutionProgressData:
		s.emit("tool_call_update", map[string]any{"toolCallId": d.ToolCallID, "status": "in_progress", "content": chat.Text(d.ProgressMessage)}, "")
	case *rpc.AssistantUsageData:
		data := map[string]any{}
		if d.InputTokens != nil {
			data["inputTokens"] = *d.InputTokens
		}
		if d.OutputTokens != nil {
			data["outputTokens"] = *d.OutputTokens
		}
		if d.CacheReadTokens != nil {
			data["cacheReadTokens"] = *d.CacheReadTokens
		}
		s.emit("usage", data, "")
	case *rpc.SessionErrorData:
		s.emit("error", map[string]any{"code": "session_error", "message": "Copilot session failed", "fatal": true}, "")
	case *rpc.AssistantReasoningData, *rpc.AssistantReasoningDeltaData:
		// Deliberately excluded: hidden reasoning is not durable user content.
	default:
		s.emit("protocol_unknown", map[string]any{"driver": "copilot-sdk", "kind": "field", "name": string(e.Type())}, "")
	}
}

func (s *session) finishTurn(aborted bool) {
	s.mu.Lock()
	active := s.busy
	reason := "end_turn"
	if aborted || s.interrupted {
		reason = "cancelled"
		s.interrupted = true
	}
	s.busy = false
	s.cancelRequestsLocked()
	s.mu.Unlock()
	if active {
		s.emit("turn_ended", map[string]any{"reason": reason, "settled": true}, "")
	}
}
