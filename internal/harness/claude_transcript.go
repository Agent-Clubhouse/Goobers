package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/goobers/goobers/internal/telemetry"
)

const maxClaudeStreamEventBytes = DefaultMaxTranscriptBytes

type claudeStreamMessage struct {
	Type         string                      `json:"type"`
	Subtype      string                      `json:"subtype"`
	Model        string                      `json:"model"`
	Message      *claudeMessage              `json:"message"`
	Result       string                      `json:"result"`
	Usage        claudeUsage                 `json:"usage"`
	ModelUsage   map[string]claudeModelUsage `json:"modelUsage"`
	TotalCostUSD *float64                    `json:"total_cost_usd"`
}

type claudeMessage struct {
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   *bool           `json:"is_error"`
}

type claudeUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

type claudeModelUsage struct {
	InputTokens  *int64   `json:"inputTokens"`
	OutputTokens *int64   `json:"outputTokens"`
	CostUSD      *float64 `json:"costUSD"`
}

type claudeUsageAccumulator struct {
	inputTokens  int64
	outputTokens int64
	costUSD      float64
	hasInput     bool
	hasOutput    bool
	hasCost      bool
}

func convertClaudeStream(r io.Reader, prompts []string, limit, alreadyDropped int64) (transcriptCapture, bool) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), int(maxClaudeStreamEventBytes))
	buf := newTranscriptBuffer(limit)
	converted := false
	promptIndex := 0
	var finalOutput *transcriptEvent
	aggregate := claudeUsageAccumulator{}
	models := make(map[string]*claudeUsageAccumulator)

	writeEvents := func(events ...transcriptEvent) bool {
		for _, event := range events {
			encoded, err := marshalTranscriptEvents(event)
			if err != nil {
				return false
			}
			_, _ = buf.Write(encoded)
			if event.Role == "assistant" && event.Content != "" {
				captured := event
				finalOutput = &captured
			}
		}
		return true
	}
	writePrompt := func() bool {
		if promptIndex >= len(prompts) {
			return true
		}
		ok := writeEvents(transcriptEvent{Role: "user", Content: prompts[promptIndex]})
		promptIndex++
		return ok
	}

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var native claudeStreamMessage
		if json.Unmarshal(line, &native) != nil {
			continue
		}
		events, recognized := convertClaudeMessage(native)
		if !recognized {
			continue
		}
		if !converted || native.Type == "system" && native.Subtype == "init" {
			if !writePrompt() {
				return transcriptCapture{}, false
			}
		}
		if native.Type == "result" {
			accumulateClaudeUsage(&aggregate, native.Usage.InputTokens, native.Usage.OutputTokens, native.TotalCostUSD)
			names := make([]string, 0, len(native.ModelUsage))
			for name := range native.ModelUsage {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				usage := native.ModelUsage[name]
				accumulator := models[name]
				if accumulator == nil {
					accumulator = &claudeUsageAccumulator{}
					models[name] = accumulator
				}
				accumulateClaudeUsage(accumulator, usage.InputTokens, usage.OutputTokens, usage.CostUSD)
			}
		}
		if !writeEvents(events...) {
			return transcriptCapture{}, false
		}
		converted = true
	}
	if scanner.Err() != nil || !converted {
		return transcriptCapture{}, false
	}

	for promptIndex < len(prompts) {
		if !writePrompt() {
			return transcriptCapture{}, false
		}
	}
	floor := make([]transcriptEvent, 0, 2)
	if len(prompts) > 0 {
		floor = append(floor, transcriptEvent{Role: "user", Content: prompts[0]})
	}
	if finalOutput != nil {
		floor = append(floor, *finalOutput)
	}
	if alreadyDropped > 0 && !buf.Truncated() {
		marker, err := marshalTranscriptEvents(transcriptEvent{
			Role:      "system",
			Content:   fmt.Sprintf("[transcript truncated: %d bytes dropped]", alreadyDropped),
			Truncated: true,
		})
		if err != nil {
			return transcriptCapture{}, false
		}
		_, _ = buf.Write(marker)
	}
	data, dropped, err := finalizeCanonicalTranscript(buf, floor, alreadyDropped)
	if err != nil {
		return transcriptCapture{}, false
	}
	return transcriptCapture{
		data:         data,
		metrics:      claudeMetrics(aggregate),
		modelUsage:   claudeModelUsages(models),
		truncated:    dropped > 0,
		droppedBytes: dropped,
	}, true
}

func convertClaudeMessage(native claudeStreamMessage) ([]transcriptEvent, bool) {
	switch native.Type {
	case "system":
		if native.Subtype != "init" {
			return nil, false
		}
		model := native.Model
		if model == "" && native.Message != nil {
			model = native.Message.Model
		}
		if model == "" {
			return nil, native.Subtype == "init"
		}
		return []transcriptEvent{{Role: "system", Model: model}}, true
	case "assistant":
		if native.Message == nil {
			return nil, false
		}
		blocks, ok := decodeClaudeContent(native.Message.Content)
		if !ok {
			return nil, false
		}
		events := make([]transcriptEvent, 0, len(blocks))
		for _, block := range blocks {
			switch block.Type {
			case "text":
				events = append(events, transcriptEvent{Role: "assistant", Content: block.Text, Model: native.Message.Model})
			case "tool_use", "server_tool_use":
				if block.ID == "" || block.Name == "" {
					continue
				}
				events = append(events, transcriptEvent{
					Role:  "assistant",
					Model: native.Message.Model,
					ToolCall: &transcriptTool{
						ID:        block.ID,
						Name:      block.Name,
						Arguments: block.Input,
					},
				})
			}
		}
		return events, true
	case "user":
		if native.Message == nil {
			return nil, false
		}
		if text, ok := decodeClaudeString(native.Message.Content); ok {
			return []transcriptEvent{{Role: "user", Content: text}}, true
		}
		blocks, ok := decodeClaudeContent(native.Message.Content)
		if !ok {
			return nil, false
		}
		events := make([]transcriptEvent, 0, len(blocks))
		for _, block := range blocks {
			switch block.Type {
			case "text":
				events = append(events, transcriptEvent{Role: "user", Content: block.Text})
			case "tool_result":
				if block.ToolUseID == "" {
					continue
				}
				success := block.IsError == nil || !*block.IsError
				events = append(events, transcriptEvent{
					Role:    "tool",
					Content: claudeBlockContent(block.Content),
					ToolCall: &transcriptTool{
						ID:      block.ToolUseID,
						Success: &success,
					},
				})
			}
		}
		return events, true
	case "result":
		events := make([]transcriptEvent, 0, len(native.ModelUsage)+2)
		if native.Result != "" {
			events = append(events, transcriptEvent{Role: "assistant", Content: native.Result})
		}
		names := make([]string, 0, len(native.ModelUsage))
		for name := range native.ModelUsage {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			usage := native.ModelUsage[name]
			events = append(events, transcriptEvent{
				Role:  "assistant",
				Model: name,
				Usage: &transcriptUsage{
					InputTokens:  usage.InputTokens,
					OutputTokens: usage.OutputTokens,
					Cost:         usage.CostUSD,
				},
			})
		}
		if len(native.ModelUsage) == 0 &&
			(native.Usage.InputTokens != nil || native.Usage.OutputTokens != nil || native.TotalCostUSD != nil) {
			events = append(events, transcriptEvent{
				Role: "assistant",
				Usage: &transcriptUsage{
					InputTokens:  native.Usage.InputTokens,
					OutputTokens: native.Usage.OutputTokens,
					Cost:         native.TotalCostUSD,
				},
			})
		}
		return events, true
	default:
		return nil, false
	}
}

func decodeClaudeContent(raw json.RawMessage) ([]claudeContentBlock, bool) {
	var blocks []claudeContentBlock
	if len(raw) == 0 || json.Unmarshal(raw, &blocks) != nil {
		return nil, false
	}
	return blocks, true
}

func decodeClaudeString(raw json.RawMessage) (string, bool) {
	var text string
	if len(raw) == 0 || json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

func claudeBlockContent(raw json.RawMessage) string {
	if text, ok := decodeClaudeString(raw); ok {
		return text
	}
	blocks, ok := decodeClaudeContent(raw)
	if !ok {
		return string(raw)
	}
	var out bytes.Buffer
	for _, block := range blocks {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		if out.Len() > 0 {
			_ = out.WriteByte('\n')
		}
		out.WriteString(block.Text)
	}
	return out.String()
}

func accumulateClaudeUsage(accumulator *claudeUsageAccumulator, input, output *int64, cost *float64) {
	if input != nil {
		accumulator.inputTokens += *input
		accumulator.hasInput = true
	}
	if output != nil {
		accumulator.outputTokens += *output
		accumulator.hasOutput = true
	}
	if cost != nil {
		accumulator.costUSD += *cost
		accumulator.hasCost = true
	}
}

func claudeMetrics(usage claudeUsageAccumulator) map[string]float64 {
	metrics := make(map[string]float64, 3)
	if usage.hasInput {
		metrics[telemetry.AttrGenAIUsageInputTokens] = float64(usage.inputTokens)
	}
	if usage.hasOutput {
		metrics[telemetry.AttrGenAIUsageOutputTokens] = float64(usage.outputTokens)
	}
	if usage.hasCost {
		metrics[telemetry.AttrUsageCostUSD] = usage.costUSD
	}
	if len(metrics) == 0 {
		return nil
	}
	return metrics
}

func claudeModelUsages(models map[string]*claudeUsageAccumulator) []telemetry.ModelUsage {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	usages := make([]telemetry.ModelUsage, 0, len(names))
	for _, name := range names {
		accumulator := models[name]
		usage := telemetry.ModelUsage{Model: name}
		if accumulator.hasInput {
			value := accumulator.inputTokens
			usage.InputTokens = &value
		}
		if accumulator.hasOutput {
			value := accumulator.outputTokens
			usage.OutputTokens = &value
		}
		if accumulator.hasCost {
			value := accumulator.costUSD
			usage.CostUSD = &value
		}
		usages = append(usages, usage)
	}
	return usages
}
