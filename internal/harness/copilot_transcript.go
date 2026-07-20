package harness

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/telemetry"
)

const maxCopilotSessionEventBytes = DefaultMaxTranscriptBytes

// Copilot reports billing in nano-AI units: 1e9 nano-AIU is one $0.01 AI credit.
const nanoAIUPerUSD = 100_000_000_000

type transcriptEvent struct {
	Role     string           `json:"role"`
	Content  string           `json:"content,omitempty"`
	Model    string           `json:"model,omitempty"`
	Usage    *transcriptUsage `json:"usage,omitempty"`
	ToolCall *transcriptTool  `json:"tool_call,omitempty"`
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

type copilotSessionEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type copilotUserMessageData struct {
	Content *string `json:"content"`
}

type copilotAssistantMessageData struct {
	MessageID *string `json:"messageId"`
	Content   *string `json:"content"`
	Model     string  `json:"model"`
}

type copilotToolStartData struct {
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Arguments  json.RawMessage `json:"arguments"`
	Model      string          `json:"model"`
}

type copilotToolCompleteData struct {
	ToolCallID string `json:"toolCallId"`
	Success    *bool  `json:"success"`
	Model      string `json:"model"`
	Result     *struct {
		Content string `json:"content"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type copilotModelChangeData struct {
	NewModel string `json:"newModel"`
}

type copilotErrorData struct {
	Message string `json:"message"`
}

type copilotShutdownData struct {
	TotalPremiumRequests *float64 `json:"totalPremiumRequests"`
	TotalNanoAIU         *float64 `json:"totalNanoAiu"`
	ModelMetrics         map[string]struct {
		Requests struct {
			Count *int64   `json:"count"`
			Cost  *float64 `json:"cost"`
		} `json:"requests"`
		Usage struct {
			InputTokens      *int64 `json:"inputTokens"`
			OutputTokens     *int64 `json:"outputTokens"`
			CacheReadTokens  *int64 `json:"cacheReadTokens"`
			CacheWriteTokens *int64 `json:"cacheWriteTokens"`
			ReasoningTokens  *int64 `json:"reasoningTokens"`
		} `json:"usage"`
		TotalNanoAIU *float64 `json:"totalNanoAiu"`
	} `json:"modelMetrics"`
}

type transcriptCapture struct {
	data         []byte
	metrics      map[string]float64
	truncated    bool
	droppedBytes int64
}

func readCopilotSessionTranscript(path string, limit int64) (transcriptCapture, bool) {
	f, err := os.Open(path)
	if err != nil {
		return transcriptCapture{}, false
	}
	capture, ok := convertCopilotSessionEvents(f, limit)
	if err := f.Close(); err != nil {
		return transcriptCapture{}, false
	}
	return capture, ok
}

func convertCopilotSessionEvents(r io.Reader, limit int64) (transcriptCapture, bool) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), int(maxCopilotSessionEventBytes))
	trailingPartial := false
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		advance, token, err = bufio.ScanLines(data, atEOF)
		trailingPartial = atEOF && advance == len(data) && len(data) > 0 && data[len(data)-1] != '\n'
		return advance, token, err
	})
	buf := newTranscriptBuffer(limit)
	converted := false
	var metrics map[string]float64
	var prompt, finalOutput *transcriptEvent

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var native copilotSessionEvent
		if err := json.Unmarshal(line, &native); err != nil {
			if trailingPartial {
				break
			}
			return transcriptCapture{}, false
		}
		events := convertCopilotSessionEvent(native)
		if native.Type == "session.shutdown" {
			if usage, ok := copilotUsageMetrics(native.Data); ok {
				metrics = usage
				if len(usage) > 0 {
					converted = true
				}
			}
		}
		for _, event := range events {
			if native.Type == "user.message" && prompt == nil {
				captured := event
				prompt = &captured
			}
			if native.Type == "assistant.message" {
				captured := event
				finalOutput = &captured
			}
			encoded, err := marshalTranscriptEvents(event)
			if err != nil {
				return transcriptCapture{}, false
			}
			_, _ = buf.Write(encoded)
			converted = true
		}
	}
	if scanner.Err() != nil {
		return transcriptCapture{}, false
	}
	if !converted {
		return transcriptCapture{}, false
	}
	floor := make([]transcriptEvent, 0, 2)
	if prompt != nil {
		floor = append(floor, *prompt)
	}
	if finalOutput != nil {
		floor = append(floor, *finalOutput)
	}
	data, dropped, err := finalizeCanonicalTranscript(buf, floor, 0)
	if err != nil {
		return transcriptCapture{}, false
	}
	return transcriptCapture{
		data:         data,
		metrics:      metrics,
		truncated:    dropped > 0,
		droppedBytes: dropped,
	}, true
}

func copilotUsageMetrics(raw json.RawMessage) (map[string]float64, bool) {
	var data copilotShutdownData
	if json.Unmarshal(raw, &data) != nil {
		return nil, false
	}

	models := make([]string, 0, len(data.ModelMetrics))
	for model := range data.ModelMetrics {
		models = append(models, model)
	}
	sort.Strings(models)

	metrics := make(map[string]float64, 4)
	var premiumRequests, nanoAIU float64
	var hasPremiumRequests, hasNanoAIU bool
	for _, model := range models {
		metric := data.ModelMetrics[model]
		if metric.Usage.InputTokens != nil {
			metrics[telemetry.AttrGenAIUsageInputTokens] += float64(*metric.Usage.InputTokens)
		}
		if metric.Usage.OutputTokens != nil {
			metrics[telemetry.AttrGenAIUsageOutputTokens] += float64(*metric.Usage.OutputTokens)
		}
		if metric.Requests.Cost != nil {
			premiumRequests += *metric.Requests.Cost
			hasPremiumRequests = true
		}
		if metric.TotalNanoAIU != nil {
			nanoAIU += *metric.TotalNanoAIU
			hasNanoAIU = true
		}
	}
	if data.TotalPremiumRequests != nil {
		premiumRequests = *data.TotalPremiumRequests
		hasPremiumRequests = true
	}
	if data.TotalNanoAIU != nil {
		nanoAIU = *data.TotalNanoAIU
		hasNanoAIU = true
	}
	if hasPremiumRequests {
		metrics[telemetry.AttrCopilotPremiumRequests] = premiumRequests
	}
	if hasNanoAIU {
		metrics[telemetry.AttrUsageCostUSD] = nanoAIU / nanoAIUPerUSD
	}
	if len(metrics) == 0 {
		return nil, true
	}
	return metrics, true
}

func convertCopilotSessionEvent(native copilotSessionEvent) []transcriptEvent {
	switch native.Type {
	case "user.message":
		var data copilotUserMessageData
		if json.Unmarshal(native.Data, &data) != nil || data.Content == nil {
			return nil
		}
		return []transcriptEvent{{Role: "user", Content: *data.Content}}
	case "assistant.message":
		var data copilotAssistantMessageData
		if json.Unmarshal(native.Data, &data) != nil || data.MessageID == nil || data.Content == nil {
			return nil
		}
		return []transcriptEvent{{Role: "assistant", Content: *data.Content, Model: data.Model}}
	case "tool.execution_start":
		var data copilotToolStartData
		if json.Unmarshal(native.Data, &data) != nil || data.ToolCallID == "" || data.ToolName == "" {
			return nil
		}
		return []transcriptEvent{{
			Role:  "assistant",
			Model: data.Model,
			ToolCall: &transcriptTool{
				ID:        data.ToolCallID,
				Name:      data.ToolName,
				Arguments: data.Arguments,
			},
		}}
	case "tool.execution_complete":
		var data copilotToolCompleteData
		if json.Unmarshal(native.Data, &data) != nil || data.ToolCallID == "" || data.Success == nil {
			return nil
		}
		content := ""
		if data.Result != nil {
			content = data.Result.Content
		} else if data.Error != nil {
			content = data.Error.Message
		}
		return []transcriptEvent{{
			Role:    "tool",
			Content: content,
			Model:   data.Model,
			ToolCall: &transcriptTool{
				ID:      data.ToolCallID,
				Success: data.Success,
			},
		}}
	case "session.model_change":
		var data copilotModelChangeData
		if json.Unmarshal(native.Data, &data) != nil || data.NewModel == "" {
			return nil
		}
		return []transcriptEvent{{Role: "system", Model: data.NewModel}}
	case "session.error":
		var data copilotErrorData
		if json.Unmarshal(native.Data, &data) != nil || data.Message == "" {
			return nil
		}
		return []transcriptEvent{{Role: "system", Content: data.Message}}
	case "session.shutdown":
		var data copilotShutdownData
		if json.Unmarshal(native.Data, &data) != nil {
			return nil
		}
		models := make([]string, 0, len(data.ModelMetrics))
		for model := range data.ModelMetrics {
			models = append(models, model)
		}
		sort.Strings(models)
		events := make([]transcriptEvent, 0, len(models))
		for _, model := range models {
			metric := data.ModelMetrics[model]
			events = append(events, transcriptEvent{
				Role:  "assistant",
				Model: model,
				Usage: &transcriptUsage{
					InputTokens:      metric.Usage.InputTokens,
					OutputTokens:     metric.Usage.OutputTokens,
					CacheReadTokens:  metric.Usage.CacheReadTokens,
					CacheWriteTokens: metric.Usage.CacheWriteTokens,
					ReasoningTokens:  metric.Usage.ReasoningTokens,
					Requests:         metric.Requests.Count,
					Cost:             metric.Requests.Cost,
					NanoAIU:          metric.TotalNanoAIU,
				},
			})
		}
		return events
	default:
		return nil
	}
}

func newCopilotCaptureID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return strings.Join([]string{
		hex.EncodeToString(id[0:4]),
		hex.EncodeToString(id[4:6]),
		hex.EncodeToString(id[6:8]),
		hex.EncodeToString(id[8:10]),
		hex.EncodeToString(id[10:16]),
	}, "-"), nil
}

func copilotSessionLogPath(home, sessionID string) string {
	return filepath.Join(home, "session-state", sessionID, "events.jsonl")
}

func copilotConfigHome(env []string) (string, bool) {
	var home string
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch name {
		case "COPILOT_HOME":
			if value != "" {
				return value, true
			}
		case "HOME":
			home = value
		}
	}
	if home == "" {
		return "", false
	}
	return filepath.Join(home, ".copilot"), true
}

func copilotCommandSelectsSession(argv []string) bool {
	for _, arg := range argv {
		switch {
		case arg == "--session-id", strings.HasPrefix(arg, "--session-id="):
			return true
		case arg == "--resume", arg == "-r", strings.HasPrefix(arg, "--resume="), strings.HasPrefix(arg, "-r="):
			return true
		case arg == "--continue":
			return true
		case arg == "--connect", strings.HasPrefix(arg, "--connect="):
			return true
		}
	}
	return false
}
