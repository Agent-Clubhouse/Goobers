package harness

import (
	"encoding/json"
	"fmt"

	"github.com/goobers/goobers/internal/telemetry"
)

// codexUsage matches exec's cumulative thread usage snapshot. Pointers keep an
// omitted measure distinct from a measured zero; exec does not report USD cost
// or an authoritative per-model breakdown.
type codexUsage struct {
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheRead  *int64 `json:"cached_input_tokens"`
	CacheWrite *int64 `json:"cache_write_input_tokens"`
	Reasoning  *int64 `json:"reasoning_output_tokens"`
}

func (u codexUsage) metrics() map[string]float64 {
	result := map[string]float64{}
	for name, value := range map[string]*int64{
		telemetry.AttrGenAIUsageInputTokens:  u.Input,
		telemetry.AttrGenAIUsageOutputTokens: u.Output,
		telemetry.AttrUsageCacheReadTokens:   u.CacheRead,
		telemetry.AttrUsageCacheWriteTokens:  u.CacheWrite,
		telemetry.AttrUsageReasoningTokens:   u.Reasoning,
	} {
		if value != nil && *value >= 0 {
			result[name] = float64(*value)
		}
	}
	return result
}

// codexTranscript consumes stdout records while the process runs, independently
// of the bounded combined stdout/stderr transcript. Only one final cumulative
// usage record is emitted for the stage, so resume snapshots are never summed
// a second time by canonical transcript readers.
type codexTranscript struct {
	buffer     *syncBuffer
	model      string
	invocation int
	items      map[string]codexTranscriptItemState
	prompt     *transcriptEvent
	output     *transcriptEvent
}

type codexTranscriptItemState struct{ started, completed bool }

type codexTranscriptItem struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Command   string          `json:"command"`
	Output    string          `json:"aggregated_output"`
	ExitCode  *int            `json:"exit_code"`
	Status    string          `json:"status"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"`
	Error     json.RawMessage `json:"error"`
	Changes   json.RawMessage `json:"changes"`
	Query     string          `json:"query"`
	Action    json.RawMessage `json:"action"`
	Results   json.RawMessage `json:"results"`
}

func newCodexTranscript(limit int64, model string) *codexTranscript {
	return &codexTranscript{buffer: newTranscriptBuffer(limit), model: model}
}

func (c *codexTranscript) beginTurn(prompt string) {
	c.invocation++
	c.items = map[string]codexTranscriptItemState{}
	event := transcriptEvent{Role: "user", Content: prompt}
	c.prompt = &event
	c.write(event)
}

func (c *codexTranscript) write(events ...transcriptEvent) {
	data, err := marshalTranscriptEvents(events...)
	if err == nil {
		_, _ = c.buffer.Write(data)
	}
}

func (c *codexTranscript) observe(raw []byte) {
	var event struct {
		Type string              `json:"type"`
		Item codexTranscriptItem `json:"item"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	// Once the output bound is reached, stop retaining item identities too.
	// Keep the latest completed assistant message as a bounded transcript floor.
	if c.buffer.Truncated() {
		if event.Type == "item.completed" && event.Item.Type == "agent_message" {
			c.rememberOutput(event.Item.Text)
		}
		_, _ = c.buffer.Write(raw)
		return
	}
	if normalized, ok := normalizedAgentRecord(raw); ok {
		_, _ = c.buffer.Write(append(normalized, '\n'))
		return
	}
	switch event.Type {
	case "turn.completed":
		// The final stage snapshot is emitted by finish after repair accounting.
		return
	case "item.started", "item.updated", "item.completed":
		if c.observeItem(event.Type, event.Item) {
			return
		}
	}
	// Unknown/native-only records remain inspectable without pretending to be
	// canonical tool calls or complete child-agent lifecycle telemetry.
	c.write(transcriptEvent{Role: "system", Content: "[codex event] " + string(raw)})
}

func (c *codexTranscript) rememberOutput(text string) {
	// Avoid retaining a large final message outside the transcript byte bound.
	if int64(len(text)) > c.buffer.limit {
		text = text[:c.buffer.limit]
	}
	c.output = &transcriptEvent{Role: "assistant", Content: text, Model: c.model}
}

func (c *codexTranscript) observeItem(kind string, item codexTranscriptItem) bool {
	if item.ID == "" {
		return false
	}
	state := c.items[item.ID]
	if item.Type == "agent_message" {
		if kind == "item.completed" && !state.completed {
			c.rememberOutput(item.Text)
			c.write(transcriptEvent{Role: "assistant", Content: item.Text, Model: c.model})
			state.completed = true
			c.items[item.ID] = state
		}
		return true
	}
	name, args, content, success, ok := codexTranscriptTool(item)
	if !ok {
		return false
	}
	id := fmt.Sprintf("codex-%d:%s", c.invocation, item.ID)
	if !state.started {
		c.write(transcriptEvent{Role: "assistant", Model: c.model, ToolCall: &transcriptTool{ID: id, Name: name, Arguments: args}})
		state.started = true
	}
	if kind == "item.completed" && !state.completed {
		c.write(transcriptEvent{Role: "tool", Content: content, Model: c.model, ToolCall: &transcriptTool{ID: id, Name: name, Success: success}})
		state.completed = true
	}
	c.items[item.ID] = state
	return true
}

func codexTranscriptTool(item codexTranscriptItem) (string, json.RawMessage, string, *bool, bool) {
	var arguments any
	var name, content string
	var success *bool
	switch item.Status {
	case "completed":
		value := true
		success = &value
	case "failed", "declined":
		value := false
		success = &value
	}
	switch item.Type {
	case "command_execution":
		name = "command_execution"
		arguments = map[string]string{"command": item.Command}
		content = item.Output
		if item.ExitCode != nil {
			value := *item.ExitCode == 0
			success = &value
		}
	case "mcp_tool_call":
		name = "mcp__" + item.Server + "__" + item.Tool
		arguments = item.Arguments
		content = string(item.Result)
		if len(item.Error) > 0 && string(item.Error) != "null" {
			content = string(item.Error)
			value := false
			success = &value
		}
	case "file_change":
		name = "apply_patch"
		arguments = map[string]json.RawMessage{"changes": item.Changes}
		content = string(item.Changes)
	case "web_search":
		name = "web_search"
		arguments = map[string]any{"query": item.Query, "action": item.Action}
		content = string(item.Results)
		// The exec web-search item has no success field. Completion is not proof
		// that the remote search succeeded; leave success unknown.
	default:
		return "", nil, "", nil, false
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return "", nil, "", nil, false
	}
	return name, encoded, content, success, true
}

func codexTranscriptUsage(metrics map[string]float64) *transcriptUsage {
	pointer := func(name string) *int64 {
		value, ok := metrics[name]
		if !ok {
			return nil
		}
		n := int64(value)
		return &n
	}
	usage := &transcriptUsage{
		InputTokens: pointer(telemetry.AttrGenAIUsageInputTokens), OutputTokens: pointer(telemetry.AttrGenAIUsageOutputTokens),
		CacheReadTokens: pointer(telemetry.AttrUsageCacheReadTokens), CacheWriteTokens: pointer(telemetry.AttrUsageCacheWriteTokens), ReasoningTokens: pointer(telemetry.AttrUsageReasoningTokens),
	}
	if usage.InputTokens == nil && usage.OutputTokens == nil && usage.CacheReadTokens == nil && usage.CacheWriteTokens == nil && usage.ReasoningTokens == nil {
		return nil
	}
	return usage
}

func (c *codexTranscript) finish(metrics map[string]float64, dropped int64) (transcriptCapture, error) {
	var floor []transcriptEvent
	if c.prompt != nil {
		floor = append(floor, *c.prompt)
	}
	if c.output != nil {
		floor = append(floor, *c.output)
	}
	if usage := codexTranscriptUsage(metrics); usage != nil {
		event := transcriptEvent{Role: "assistant", Model: c.model, Usage: usage}
		c.write(event)
		floor = append(floor, event)
	}
	data, dropped, err := finalizeCanonicalTranscript(c.buffer, floor, dropped)
	return transcriptCapture{data: data, truncated: dropped > 0, droppedBytes: dropped}, err
}
