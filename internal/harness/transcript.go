package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

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

func boundCanonicalTranscript(data []byte, limit, alreadyDropped int64) ([]byte, int64, error) {
	if limit <= 0 {
		limit = DefaultMaxTranscriptBytes
	}
	if int64(len(data)) <= limit {
		return data, alreadyDropped, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	var events []transcriptEvent
	for {
		var event transcriptEvent
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			return nil, 0, err
		}
		events = append(events, event)
	}

	retained, err := truncateTranscriptContents(events, limit)
	if err != nil {
		return nil, 0, err
	}
	dropped := alreadyDropped + int64(len(data)-len(retained))
	marker, err := marshalTranscriptEvents(transcriptEvent{
		Role:      "system",
		Content:   fmt.Sprintf("[transcript truncated: %d bytes dropped]", dropped),
		Truncated: true,
	})
	if err != nil {
		return nil, 0, err
	}
	return append(retained, marker...), dropped, nil
}

func truncateTranscriptContents(events []transcriptEvent, limit int64) ([]byte, error) {
	contents := make([][]rune, len(events))
	maxRunes := 0
	lastAssistant := -1
	for i := range events {
		contents[i] = []rune(events[i].Content)
		if len(contents[i]) > maxRunes {
			maxRunes = len(contents[i])
		}
		if events[i].Role == "assistant" {
			lastAssistant = i
		}
	}

	encode := func(contentRunes int, forceTruncated bool) ([]byte, error) {
		candidate := append([]transcriptEvent(nil), events...)
		for i := range candidate {
			if len(contents[i]) > contentRunes {
				candidate[i].Content = string(contents[i][:contentRunes])
			}
			if forceTruncated && len(contents[i]) > 0 {
				candidate[i].Truncated = true
			} else if len(contents[i]) > contentRunes || i == lastAssistant {
				candidate[i].Truncated = true
			}
		}
		return marshalTranscriptEvents(candidate...)
	}

	// A shared rune cap gives both prompt and final output a retained prefix;
	// shorter content is preserved in full before either record can disappear.
	bestRunes := -1
	for low, high := 0, maxRunes; low <= high; {
		mid := low + (high-low)/2
		candidate, err := encode(mid, true)
		if err != nil {
			return nil, err
		}
		if int64(len(candidate)) <= limit {
			bestRunes = mid
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if bestRunes < 0 {
		return nil, nil
	}
	return encode(bestRunes, false)
}

func finalizeCanonicalTranscript(buf *syncBuffer, floor []transcriptEvent, alreadyDropped int64) ([]byte, int64, error) {
	retained := buf.retainedBytes()
	if !buf.Truncated() {
		return retained, alreadyDropped, nil
	}

	end := bytes.LastIndexByte(retained, '\n') + 1
	bounded := retained[:end]
	for start := 0; start < len(floor); start++ {
		candidate, err := truncateTranscriptContents(floor[start:], buf.limit)
		if err != nil {
			return nil, 0, err
		}
		if candidate != nil {
			bounded = candidate
			break
		}
	}
	dropped := alreadyDropped + int64(len(retained)) + buf.Dropped() - int64(len(bounded))
	marker, err := marshalTranscriptEvents(transcriptEvent{
		Role:      "system",
		Content:   fmt.Sprintf("[transcript truncated: %d bytes dropped]", dropped),
		Truncated: true,
	})
	if err != nil {
		return nil, 0, err
	}
	return append(bounded, marker...), dropped, nil
}
