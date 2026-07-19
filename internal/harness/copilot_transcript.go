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
)

const maxCopilotSessionEventBytes = DefaultMaxTranscriptBytes

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
	ModelMetrics map[string]struct {
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
	truncated    bool
	droppedBytes int64
}

func readCopilotSessionTranscript(home string, limit int64) (transcriptCapture, bool) {
	path, ok := findCopilotSessionLog(home)
	if !ok {
		return transcriptCapture{}, false
	}
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
	buf := newTranscriptBuffer(limit)
	converted := false

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var native copilotSessionEvent
		if err := json.Unmarshal(line, &native); err != nil {
			break
		}
		events := convertCopilotSessionEvent(native)
		for _, event := range events {
			encoded, err := json.Marshal(event)
			if err != nil {
				return transcriptCapture{}, false
			}
			_, _ = buf.Write(encoded)
			_, _ = buf.Write([]byte{'\n'})
			converted = true
		}
	}
	if !converted {
		return transcriptCapture{}, false
	}
	return transcriptCapture{
		data:         buf.Bytes(),
		truncated:    buf.Truncated(),
		droppedBytes: buf.Dropped(),
	}, true
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

func findCopilotSessionLog(home string) (string, bool) {
	entries, err := os.ReadDir(filepath.Join(home, "session-state"))
	if err != nil {
		return "", false
	}
	var found string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := copilotSessionLogPath(home, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if found != "" {
			return "", false
		}
		found = path
	}
	return found, found != ""
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
