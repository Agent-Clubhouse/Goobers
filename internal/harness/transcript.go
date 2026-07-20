package harness

import (
	"bytes"
	"encoding/json"

	"github.com/goobers/goobers/internal/telemetry"
)

type transcriptEvent struct {
	Schema    string           `json:"schema"`
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	Model     string           `json:"model,omitempty"`
	Usage     *transcriptUsage `json:"usage,omitempty"`
	ToolCall  *transcriptTool  `json:"tool_call,omitempty"`
	Truncated bool             `json:"truncated,omitempty"`
}

type transcriptUsage struct {
	InputTokens      *int64   `json:"input_tokens,omitempty"`
	OutputTokens     *int64   `json:"output_tokens,omitempty"`
	CacheReadTokens  *int64   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64   `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  *int64   `json:"reasoning_tokens,omitempty"`
	Requests         *int64   `json:"requests,omitempty"`
	Cost             *float64 `json:"cost,omitempty"`
	NanoAIU          *float64 `json:"nano_aiu,omitempty"`
}

type transcriptTool struct {
	ID        string          `json:"id"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Success   *bool           `json:"success,omitempty"`
}

func marshalTranscriptEvents(events ...transcriptEvent) ([]byte, error) {
	var out bytes.Buffer
	for _, event := range events {
		event.Schema = telemetry.GenAIEventSchema
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		_, _ = out.Write(encoded)
		_ = out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func composedTranscript(prompt string, output []byte, model string, truncated bool) ([]byte, error) {
	return marshalTranscriptEvents(
		transcriptEvent{Role: "user", Content: prompt},
		transcriptEvent{Role: "assistant", Content: string(output), Model: model, Truncated: truncated},
	)
}
