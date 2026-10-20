package interactiveagent

import (
	"testing"

	"github.com/goobers/goobers/internal/chat"
)

func TestProjectChatEvent(t *testing.T) {
	for _, kind := range []string{"agent_thought", "agent_thought_chunk"} {
		if _, ok := ProjectChatEvent(chat.Event{Type: kind, Data: map[string]any{"content": chat.Text("private reasoning")}}); ok {
			t.Fatalf("reasoning %s was projected", kind)
		}
	}
	event := chat.Event{V: 2, AgentID: "agent", SessionID: "session", TurnID: "turn", Seq: 4, Fence: 3, CommandID: "cmd", Type: "agent_message_chunk", Data: map[string]any{"content": chat.Text("hello")}}
	got, ok := ProjectChatEvent(event)
	if !ok || got.Type != "assistant.delta" || got.Data["text"] != "hello" {
		t.Fatalf("message projection: %#v", got)
	}
	envelope := got.Data["chat"].(map[string]any)
	if envelope["commandId"] != "cmd" || envelope["seq"] != float64(4) {
		t.Fatalf("envelope lost: %#v", envelope)
	}
	if _, ok := envelope["data"].(map[string]any)["content"].([]any); !ok {
		t.Fatal("content must be traversable by the host scrubber")
	}
	for _, status := range []string{"in_progress", "completed", "failed", "cancelled"} {
		event.Type = "tool_call_update"
		event.Data = map[string]any{"toolCallId": "tool-1", "status": status, "content": chat.Text("output")}
		got, _ = ProjectChatEvent(event)
		want := "tool.completed"
		if status == "in_progress" {
			want = "tool.output"
		}
		if got.Type != want || got.Data["toolCallId"] != "tool-1" {
			t.Fatalf("tool lifecycle: %#v", got)
		}
	}
	event.Type = "future_event"
	got, _ = ProjectChatEvent(event)
	if got.Type != "execution.note" || got.Data["chat"].(map[string]any)["type"] != "protocol_unknown" {
		t.Fatalf("unknown event was hidden: %#v", got)
	}
}
