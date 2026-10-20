package interactiveagent

import (
	"encoding/json"
	"strings"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/interactive"
)

// ProjectChatEvent adapts normalized driver events to the durable interactive
// log. The complete v2 envelope remains under data.chat; the host still assigns
// the durable sequence, session, turn and fence. Reasoning is never persisted.
func ProjectChatEvent(event chat.Event) (interactive.ExecutionEvent, bool) {
	if event.Type == "agent_thought" || event.Type == "agent_thought_chunk" {
		return interactive.ExecutionEvent{}, false
	}
	if !chat.KnownEvent(event.Type) {
		event.Data = map[string]any{"kind": "event", "name": event.Type}
		event.Type = "protocol_unknown"
	}
	// Convert typed blocks and the envelope to ordinary JSON containers so the
	// host's recursive output scrubber visits every string, including content.
	raw, err := json.Marshal(event)
	if err != nil {
		event.Type = "protocol_unknown"
		event.Data = map[string]any{"kind": "payload", "name": "unserializable event"}
		return ProjectChatEvent(event)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return interactive.ExecutionEvent{}, false
	}
	data := map[string]any{"chat": envelope}
	payload, _ := envelope["data"].(map[string]any)
	var text strings.Builder
	if blocks, ok := payload["content"].([]any); ok {
		for _, block := range blocks {
			if block, ok := block.(map[string]any); ok && block["type"] == "text" {
				if value, ok := block["text"].(string); ok {
					text.WriteString(value)
				}
			}
		}
	}
	if text.Len() > 0 {
		data["text"] = text.String()
	}
	kind := "execution.note"
	switch event.Type {
	case "agent_message_chunk":
		kind = "assistant.delta"
	case "agent_message":
		kind = "assistant.message"
	case "tool_call", "tool_call_update":
		kind = "tool.started"
		if event.Type == "tool_call_update" {
			kind = "tool.output"
		}
		switch payload["status"] {
		case "completed", "failed", "cancelled":
			kind = "tool.completed"
		}
		for _, key := range []string{"toolCallId", "status", "kind"} {
			if value, ok := payload[key]; ok {
				data[key] = value
			}
		}
		if title, ok := payload["title"]; ok {
			data["name"] = title
		}
	}
	return interactive.ExecutionEvent{Type: kind, Data: data}, true
}
