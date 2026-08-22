package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/goobers/goobers/internal/telemetry"
)

const maxClaudeStreamEventBytes = DefaultMaxTranscriptBytes

type claudeStreamMessage struct {
	Type         string                      `json:"type"`
	Subtype      string                      `json:"subtype"`
	Model        string                      `json:"model"`
	Message      *claudeMessage              `json:"message"`
	Result       string                      `json:"result"`
	Errors       []string                    `json:"errors"`
	Usage        claudeUsage                 `json:"usage"`
	ModelUsage   map[string]claudeModelUsage `json:"modelUsage"`
	TotalCostUSD *float64                    `json:"total_cost_usd"`
	// MCPServers is the per-server connection report carried by the
	// system/init event: every server registered via --mcp-config appears
	// here with its live status ("connected"/"failed"). A registered server
	// that failed to start is reported here and nowhere else — the CLI
	// otherwise proceeds silently without that server's tools (#3356). A
	// pointer so an init event that omits the field entirely (an older CLI
	// shape) stays distinguishable from an empty report: only a present
	// field may ever ground a claim that a registered server was absent.
	MCPServers *[]claudeMCPServerStatus `json:"mcp_servers"`
}

type claudeMCPServerStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
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

type claudeTerminalCapture struct {
	mu         sync.Mutex
	line       []byte
	overflowed bool
	result     []byte
}

func (c *claudeTerminalCapture) Write(p []byte) (int, error) {
	n := len(p)
	c.mu.Lock()
	defer c.mu.Unlock()

	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		fragment := p
		complete := false
		if end >= 0 {
			fragment = p[:end]
			p = p[end+1:]
			complete = true
		} else {
			p = nil
		}

		if !c.overflowed {
			if len(c.line)+len(fragment) > int(maxClaudeStreamEventBytes) {
				c.line = nil
				c.overflowed = true
			} else {
				c.line = append(c.line, fragment...)
			}
		}
		if complete {
			c.captureResult(c.line)
			c.line = nil
			c.overflowed = false
		}
	}
	return n, nil
}

func (c *claudeTerminalCapture) captureResult(line []byte) {
	if c.overflowed {
		return
	}
	line = bytes.TrimSpace(line)
	var event struct {
		Type string `json:"type"`
	}
	if len(line) == 0 || json.Unmarshal(line, &event) != nil || event.Type != "result" {
		return
	}
	c.result = append(c.result[:0], line...)
}

func (c *claudeTerminalCapture) Result() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captureResult(c.line)
	return append([]byte(nil), c.result...)
}

func claudeInvocationStreams(results []ProcessResult, captures []*claudeTerminalCapture, limit int64) []io.Reader {
	retained := make([][]byte, len(results))
	for i, result := range results {
		retained[i] = processTranscriptBytes(result)
	}
	if len(results) == 2 {
		retained[0], retained[1], _ = retainedProcessTranscripts(results[0], results[1], limit)
	}

	streams := make([]io.Reader, len(results))
	for i, raw := range retained {
		var capture *claudeTerminalCapture
		if i < len(captures) {
			capture = captures[i]
		}
		streams[i] = claudeStreamWithTerminalResult(raw, capture)
	}
	return streams
}

func claudeStreamWithTerminalResult(raw []byte, capture *claudeTerminalCapture) io.Reader {
	if capture == nil {
		return bytes.NewReader(raw)
	}
	terminal := capture.Result()
	if len(terminal) == 0 {
		return bytes.NewReader(raw)
	}

	var stream bytes.Buffer
	removed := false
	for len(raw) > 0 {
		line, rest, found := bytes.Cut(raw, []byte{'\n'})
		if !removed && bytes.Equal(bytes.TrimSpace(line), terminal) {
			removed = true
		} else {
			_, _ = stream.Write(line)
			if found {
				_ = stream.WriteByte('\n')
			}
		}
		if !found {
			break
		}
		raw = rest
	}
	if stream.Len() > 0 {
		data := stream.Bytes()
		if data[len(data)-1] != '\n' {
			_ = stream.WriteByte('\n')
		}
	}
	_, _ = stream.Write(terminal)
	_ = stream.WriteByte('\n')
	return bytes.NewReader(stream.Bytes())
}

func readClaudeStreamLine(reader *bufio.Reader) ([]byte, int64, error) {
	var line []byte
	var dropped int64
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			fragment = fragment[:len(fragment)-1]
		}
		if dropped > 0 {
			dropped += int64(len(fragment))
		} else if int64(len(line))+int64(len(fragment)) > maxClaudeStreamEventBytes {
			dropped = int64(len(line)) + int64(len(fragment))
			line = nil
		} else {
			line = append(line, fragment...)
		}

		switch {
		case err == nil:
			return line, dropped, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 && dropped == 0 {
				return nil, 0, io.EOF
			}
			return line, dropped, nil
		default:
			return nil, dropped, err
		}
	}
}

func convertClaudeStreams(streams []io.Reader, prompts []string, limit, alreadyDropped int64) (transcriptCapture, bool) {
	buf := newTranscriptBuffer(limit)
	converted := false
	var streamDropped int64
	var floor []transcriptEvent
	aggregate := claudeUsageAccumulator{}
	models := make(map[string]*claudeUsageAccumulator)
	var mcpServersReported bool
	var mcpServerStatus map[string]string

	writeEvents := func(events ...transcriptEvent) bool {
		for _, event := range events {
			encoded, err := marshalTranscriptEvents(event)
			if err != nil {
				return false
			}
			_, _ = buf.Write(encoded)
		}
		return true
	}

	for streamIndex, stream := range streams {
		var invocationOutput *transcriptEvent
		var invocationResult []transcriptEvent
		if streamIndex < len(prompts) {
			if !writeEvents(transcriptEvent{Role: "user", Content: prompts[streamIndex]}) {
				return transcriptCapture{}, false
			}
		}

		reader := bufio.NewReaderSize(stream, 64*1024)
		for {
			line, dropped, err := readClaudeStreamLine(reader)
			streamDropped += dropped
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return transcriptCapture{}, false
			}
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var native claudeStreamMessage
			if json.Unmarshal(line, &native) != nil {
				continue
			}
			if native.Type == "system" && native.Subtype == "init" && native.MCPServers != nil {
				// Record the CLI's own MCP connection report (#3356). Every
				// init event carrying the field counts, including a --resume
				// recovery invocation's: statuses merge by server name with
				// the latest observation winning. An init WITHOUT the field
				// records nothing — only an explicit report may later ground
				// a claim that a registered server was absent.
				mcpServersReported = true
				for _, server := range *native.MCPServers {
					if server.Name == "" {
						continue
					}
					if mcpServerStatus == nil {
						mcpServerStatus = make(map[string]string)
					}
					mcpServerStatus[server.Name] = server.Status
				}
			}
			events, recognized := convertClaudeMessage(native)
			if !recognized {
				continue
			}
			if native.Type == "result" {
				invocationResult = invocationResult[:0]
				for _, event := range events {
					if event.Content != "" {
						invocationResult = append(invocationResult, event)
					}
				}
				modelTotals := claudeUsageAccumulator{}
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
					accumulateClaudeUsage(&modelTotals, usage.InputTokens, usage.OutputTokens, nil)
				}
				if modelTotals.hasInput || modelTotals.hasOutput {
					accumulateClaudeUsageTotals(&aggregate, modelTotals)
				} else {
					accumulateClaudeUsage(&aggregate, native.Usage.InputTokens, native.Usage.OutputTokens, nil)
				}
				accumulateClaudeUsage(&aggregate, nil, nil, native.TotalCostUSD)
			}
			for _, event := range events {
				if event.Role == "assistant" && event.Content != "" {
					captured := event
					invocationOutput = &captured
				}
			}
			if !writeEvents(events...) {
				return transcriptCapture{}, false
			}
			converted = true
		}

		if streamIndex < len(prompts) {
			floor = append(floor, transcriptEvent{Role: "user", Content: prompts[streamIndex]})
		}
		if len(invocationResult) > 0 {
			floor = append(floor, invocationResult...)
		} else if invocationOutput != nil {
			floor = append(floor, *invocationOutput)
		}
	}
	for promptIndex := len(streams); promptIndex < len(prompts); promptIndex++ {
		if !writeEvents(transcriptEvent{Role: "user", Content: prompts[promptIndex]}) {
			return transcriptCapture{}, false
		}
		floor = append(floor, transcriptEvent{Role: "user", Content: prompts[promptIndex]})
	}
	if !converted {
		return transcriptCapture{}, false
	}
	alreadyDropped += streamDropped

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
		data:               data,
		metrics:            claudeMetrics(aggregate),
		modelUsage:         claudeModelUsages(models),
		truncated:          dropped > 0,
		droppedBytes:       dropped,
		mcpServersReported: mcpServersReported,
		mcpServerStatus:    mcpServerStatus,
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
		events := make([]transcriptEvent, 0, len(native.Errors)+len(native.ModelUsage)+2)
		if native.Result != "" {
			events = append(events, transcriptEvent{Role: "assistant", Content: native.Result})
		}
		for _, message := range native.Errors {
			if message != "" {
				events = append(events, transcriptEvent{Role: "system", Content: message})
			}
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

func accumulateClaudeUsageTotals(accumulator *claudeUsageAccumulator, usage claudeUsageAccumulator) {
	if usage.hasInput {
		accumulator.inputTokens += usage.inputTokens
		accumulator.hasInput = true
	}
	if usage.hasOutput {
		accumulator.outputTokens += usage.outputTokens
		accumulator.hasOutput = true
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
