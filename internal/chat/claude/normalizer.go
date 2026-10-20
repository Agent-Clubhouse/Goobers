// Package claude implements Claude Code's stream-json chat protocol.
// Adapted from Clubhouse (MIT), commit 3309665c; see README.md and testdata/LICENSE.
package claude

import (
	"strings"

	"github.com/goobers/goobers/internal/chat"
)

type object = map[string]any

type event struct {
	kind string
	data object
}
type normalizer struct {
	nativeID     string
	resumed      bool
	totalCost    *float64
	turnError    bool
	interrupting bool
	streamIDs    map[string]string
}

func obj(v any) object {
	m, _ := v.(map[string]any)
	if m == nil {
		return object{}
	}
	return m
}
func str(v any) string                  { s, _ := v.(string); return s }
func num(v any) float64                 { n, _ := v.(float64); return n }
func arr(v any) []any                   { a, _ := v.([]any); return a }
func ev(kind string, data object) event { return event{kind, data} }
func unknown(name string) event {
	return ev("protocol_unknown", object{"driver": "claude_stream_json", "kind": "field", "name": name})
}
func textContent(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var texts []string
	for _, item := range arr(v) {
		b := obj(item)
		if b["type"] == "text" {
			texts = append(texts, str(b["text"]))
		}
	}
	return strings.Join(texts, "\n")
}
func toolKind(name string) string {
	switch name {
	case "Read", "Glob", "Grep":
		return "read"
	case "Edit", "Write", "NotebookEdit":
		return "edit"
	case "Bash":
		return "execute"
	}
	return "other"
}
func (n *normalizer) mapFrame(m object) []event {
	if id := str(m["session_id"]); id != "" {
		n.nativeID = id
	}
	parent := str(m["parent_tool_use_id"])
	switch str(m["type"]) {
	case "keep_alive":
		return nil
	case "system":
		return n.mapSystem(m, parent)
	case "stream_event":
		return n.mapStream(m, parent)
	case "assistant":
		return n.mapAssistant(m, parent)
	case "user":
		return n.mapUser(m, parent)
	case "tool_progress":
		return []event{ev("tool_call_update", object{"toolCallId": str(m["tool_use_id"]), "status": "in_progress"})}
	case "result":
		return n.mapResult(m, parent)
	case "rate_limit_event":
		r := obj(m["rate_limit_info"])
		return []event{ev("usage", object{"limits": []object{{"status": str(r["status"]), "window": str(r["rateLimitType"]), "resetsAt": num(r["resetsAt"]) * 1000}}})}
	case "conversation_reset":
		return []event{ev("notice", object{"code": "conversation_reset"})}
	default:
		return []event{unknown(str(m["type"]))}
	}
}

func messageContent(parent, kind, text, id string) event {
	d := object{"content": chat.Text(text)}
	if parent != "" {
		d["parentToolCallId"] = parent
	}
	if id != "" {
		d["messageId"] = id
	}
	return ev(kind, d)
}

func (n *normalizer) mapSystem(m object, parent string) []event {
	sub := str(m["subtype"])
	switch sub {
	case "init":
		return []event{ev("session_info", object{"nativeSessionId": n.nativeID, "currentModel": str(m["model"]), "cliVersion": str(m["claude_code_version"])})}
	case "status":
		if m["status"] == "compacting" {
			return []event{ev("notice", object{"code": "compacting", "message": "Compacting context"})}
		}
		return nil
	case "task_started", "task_progress", "task_notification", "task_updated":
		status := "running"
		if sub == "task_notification" {
			status = "completed"
			if m["status"] == "failed" {
				status = "failed"
			}
			if m["status"] == "stopped" || m["status"] == "killed" {
				status = "cancelled"
			}
		}
		return []event{ev("subagent", object{"subagentId": str(m["task_id"]), "parentToolCallId": str(m["tool_use_id"]), "status": status})}
	case "compact_boundary", "api_retry", "permission_denied":
		return []event{ev("notice", object{"code": sub})}
	case "local_command_output":
		return []event{messageContent(parent, "agent_message", str(m["content"]), str(m["uuid"]))}
	case "vcs_state_changed":
		return []event{ev("workspace_changed", object{"reason": "vcs", "branch": str(m["branch"])})}
	}
	return []event{unknown("system/" + sub)}
}

func (n *normalizer) mapStream(m object, parent string) []event {
	e := obj(m["event"])
	switch str(e["type"]) {
	case "message_start":
		if n.streamIDs == nil {
			n.streamIDs = map[string]string{}
		}
		n.streamIDs[parent] = str(obj(e["message"])["id"])
		return nil
	case "content_block_start", "content_block_stop", "message_delta", "message_stop":
		return nil
	case "content_block_delta":
		d := obj(e["delta"])
		switch d["type"] {
		case "text_delta":
			return []event{messageContent(parent, "agent_message_chunk", str(d["text"]), n.streamIDs[parent])}
		case "thinking_delta":
			return []event{messageContent(parent, "agent_thought_chunk", str(d["thinking"]), n.streamIDs[parent])}
		case "input_json_delta", "signature_delta":
			return nil
		}
		return []event{unknown("stream_event/delta/" + str(d["type"]))}
	}
	return []event{unknown("stream_event/" + str(e["type"]))}
}

func (n *normalizer) mapAssistant(m object, parent string) []event {
	message := obj(m["message"])
	id := str(message["id"])
	var out []event
	if m["error"] != nil {
		n.turnError = true
	}
	for _, raw := range arr(message["content"]) {
		b := obj(raw)
		switch b["type"] {
		case "text":
			out = append(out, messageContent(parent, "agent_message", str(b["text"]), id))
		case "thinking":
			out = append(out, messageContent(parent, "agent_thought", str(b["thinking"]), id))
		case "tool_use", "server_tool_use", "mcp_tool_use":
			d := object{"toolCallId": str(b["id"]), "title": str(b["name"]), "kind": toolKind(str(b["name"])), "status": "pending"}
			if parent != "" {
				d["parentToolCallId"] = parent
			}
			out = append(out, ev("tool_call", d))
		default:
			out = append(out, unknown("assistant/content/"+str(b["type"])))
		}
	}
	return out
}

func (n *normalizer) mapUser(m object, parent string) []event {
	var out []event
	for _, raw := range arr(obj(m["message"])["content"]) {
		b := obj(raw)
		if b["type"] != "tool_result" {
			continue
		}
		status := "completed"
		if b["is_error"] == true {
			status = "failed"
		}
		d := object{"toolCallId": str(b["tool_use_id"]), "status": status, "content": chat.Text(textContent(b["content"]))}
		if parent != "" {
			d["parentToolCallId"] = parent
		}
		out = append(out, ev("tool_call_update", d))
	}
	return out // Host already emitted sent user content; do not duplicate replays.
}

func (n *normalizer) mapResult(m object, parent string) []event {
	reason := "end_turn"
	if m["is_error"] == true || m["subtype"] != "success" || n.turnError {
		reason = "error"
	}
	if n.interrupting {
		reason = "cancelled"
	}
	var out []event
	if reason == "error" {
		out = append(out, ev("error", object{"code": "turn_failed", "message": "Claude turn failed", "fatal": false}))
	}
	u := obj(m["usage"])
	usage := object{"inputTokens": num(u["input_tokens"]), "outputTokens": num(u["output_tokens"]), "cacheReadInputTokens": num(u["cache_read_input_tokens"]), "cacheCreationInputTokens": num(u["cache_creation_input_tokens"])}
	if total, ok := m["total_cost_usd"].(float64); ok {
		if n.totalCost != nil {
			delta := total - *n.totalCost
			if delta < 0 {
				delta = total
			}
			usage["costUsd"] = delta
		} else if !n.resumed {
			usage["costUsd"] = total
		}
		n.totalCost = &total
	}
	n.turnError, n.interrupting = false, false
	return append(out, ev("usage", usage), ev("turn_ended", object{"reason": reason}))
}
