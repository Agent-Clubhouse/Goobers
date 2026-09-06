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
	TotalPremiumRequests    *float64                      `json:"totalPremiumRequests"`
	TotalPremiumRequestCost *float64                      `json:"totalPremiumRequestCost"`
	TotalNanoAIU            *int64                        `json:"totalNanoAiu"`
	ModelMetrics            map[string]copilotModelMetric `json:"modelMetrics"`
	AgentMetrics            map[string]copilotAgentMetric `json:"agentMetrics"`
}

type copilotAgentMetric struct {
	TotalNanoAIU *int64                        `json:"totalNanoAiu"`
	ModelMetrics map[string]copilotModelMetric `json:"modelMetrics"`
}

type copilotModelMetric struct {
	Requests struct {
		Count *int64   `json:"count"`
		Cost  *float64 `json:"cost"`
	} `json:"requests"`
	Usage        copilotTokenUsage `json:"usage"`
	TotalNanoAIU *int64            `json:"totalNanoAiu"`
}

type copilotTokenUsage struct {
	InputTokens      *int64 `json:"inputTokens"`
	OutputTokens     *int64 `json:"outputTokens"`
	CacheReadTokens  *int64 `json:"cacheReadTokens"`
	CacheWriteTokens *int64 `json:"cacheWriteTokens"`
	ReasoningTokens  *int64 `json:"reasoningTokens"`
}

type transcriptCapture struct {
	data         []byte
	metrics      map[string]float64
	modelUsage   []telemetry.ModelUsage
	truncated    bool
	droppedBytes int64
	// finalMessage is the raw content of the LAST assistant.message in the
	// session log. Copilot does not always echo its final message to stdout
	// under --silent --output-format=text with MCP tools attached: the answer
	// lands in the session log, the harness's stdout capture stays empty, and
	// readCopilotResponseCompletion reports "final response is not valid JSON"
	// for a completion the model in fact produced correctly. Both attempts of a
	// live pr-remediation run failed this way with well-formed JSON sitting in
	// the log, discarding committed work twice per run.
	finalMessage []byte
	// mcpServersReported reports that the harness CLI emitted at least one
	// per-server MCP connection report for this session, making
	// mcpServerStatus authoritative for which registered servers actually
	// came up (#3356). Only populated by adapters whose harness reports
	// connection state (claude-code's system/init event); Copilot's session
	// transcript carries no equivalent, so its conversions leave both fields
	// zero and no availability claim is ever made from them.
	mcpServersReported bool
	// mcpServerStatus maps a registered MCP server name to the CLI-reported
	// connection status ("connected", "failed", ...). A server registered
	// for the invocation but missing from this map did not appear in the
	// CLI's report at all.
	mcpServerStatus map[string]string
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
	var modelUsage []telemetry.ModelUsage
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
		if normalized, ok := normalizedAgentRecord(line); ok {
			_, _ = buf.Write(append(normalized, '\n'))
			converted = true
		}
		events := convertCopilotSessionEvent(native)
		if native.Type == "session.shutdown" {
			if usage, models, ok := copilotUsageMetrics(native.Data, false); ok {
				metrics = usage
				modelUsage = models
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
	var finalMessage []byte
	if finalOutput != nil {
		finalMessage = []byte(finalOutput.Content)
	}
	return transcriptCapture{
		data:         data,
		metrics:      metrics,
		modelUsage:   modelUsage,
		truncated:    dropped > 0,
		droppedBytes: dropped,
		finalMessage: finalMessage,
	}, true
}

func readCopilotUsageDocument(path string) (map[string]float64, []telemetry.ModelUsage, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, false
	}
	return copilotUsageMetrics(raw, true)
}

func copilotUsageMetrics(raw json.RawMessage, authoritative bool) (map[string]float64, []telemetry.ModelUsage, bool) {
	var data copilotShutdownData
	if json.Unmarshal(raw, &data) != nil {
		return nil, nil, false
	}
	if authoritative && !validCopilotUsageDocument(data) {
		return nil, nil, false
	}

	models := make([]string, 0, len(data.ModelMetrics))
	for model := range data.ModelMetrics {
		models = append(models, model)
	}
	sort.Strings(models)

	metrics := make(map[string]float64, 7)
	var modelUsage []telemetry.ModelUsage
	var premiumRequests float64
	var nanoAIU int64
	var hasPremiumRequests, hasNanoAIU bool
	for _, model := range models {
		metric := data.ModelMetrics[model]
		usage := copilotModelUsage(model, metric, authoritative)
		addCopilotTokenMetrics(metrics, metric.Usage)
		if usage.CopilotPremiumRequests != nil {
			premiumRequests += *usage.CopilotPremiumRequests
			hasPremiumRequests = true
		}
		if usage.NanoAIU != nil {
			nanoAIU += *usage.NanoAIU
			hasNanoAIU = true
		}
		if hasCopilotModelUsage(usage) {
			modelUsage = append(modelUsage, usage)
		}
	}
	if data.TotalPremiumRequests != nil && *data.TotalPremiumRequests != 0 {
		premiumRequests = *data.TotalPremiumRequests
		hasPremiumRequests = true
	}
	if data.TotalPremiumRequestCost != nil && *data.TotalPremiumRequestCost != 0 {
		premiumRequests = *data.TotalPremiumRequestCost
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
		metrics[telemetry.AttrUsageNanoAIU] = float64(nanoAIU)
		metrics[telemetry.AttrUsageCostUSD] = telemetry.NanoAIUToUSD(nanoAIU)
	}
	if len(metrics) == 0 {
		return nil, modelUsage, true
	}
	return metrics, modelUsage, true
}

func validCopilotUsageDocument(data copilotShutdownData) bool {
	if data.TotalNanoAIU == nil || data.ModelMetrics == nil || data.AgentMetrics == nil {
		return false
	}
	var modelTotal int64
	for _, metric := range data.ModelMetrics {
		if metric.TotalNanoAIU == nil {
			return false
		}
		modelTotal += *metric.TotalNanoAIU
	}
	if modelTotal != *data.TotalNanoAIU {
		return false
	}
	for _, agent := range data.AgentMetrics {
		if agent.TotalNanoAIU == nil || agent.ModelMetrics == nil {
			return false
		}
	}
	return len(data.AgentMetrics) > 0
}

func copilotModelUsage(model string, metric copilotModelMetric, authoritative bool) telemetry.ModelUsage {
	usage := telemetry.ModelUsage{
		Model:            model,
		InputTokens:      metric.Usage.InputTokens,
		OutputTokens:     metric.Usage.OutputTokens,
		CacheReadTokens:  metric.Usage.CacheReadTokens,
		CacheWriteTokens: metric.Usage.CacheWriteTokens,
		ReasoningTokens:  metric.Usage.ReasoningTokens,
		CostBasis:        telemetry.CostBasisUnknown,
	}
	if metric.Requests.Cost != nil && *metric.Requests.Cost != 0 {
		usage.CopilotPremiumRequests = metric.Requests.Cost
		usage.BillingModel = telemetry.BillingModelPremiumRequests
	}
	if metric.TotalNanoAIU != nil {
		cost := telemetry.NanoAIUToUSD(*metric.TotalNanoAIU)
		usage.CostUSD = &cost
		usage.NanoAIU = metric.TotalNanoAIU
		usage.BillingModel = telemetry.BillingModelAICredits
		if authoritative {
			usage.CostBasis = telemetry.CostBasisVendorReported
		}
	}
	return usage
}

func addCopilotTokenMetrics(metrics map[string]float64, usage copilotTokenUsage) {
	addCopilotTokenMetric(metrics, telemetry.AttrGenAIUsageInputTokens, usage.InputTokens)
	addCopilotTokenMetric(metrics, telemetry.AttrGenAIUsageOutputTokens, usage.OutputTokens)
	addCopilotTokenMetric(metrics, telemetry.AttrUsageCacheReadTokens, usage.CacheReadTokens)
	addCopilotTokenMetric(metrics, telemetry.AttrUsageCacheWriteTokens, usage.CacheWriteTokens)
	addCopilotTokenMetric(metrics, telemetry.AttrUsageReasoningTokens, usage.ReasoningTokens)
}

func addCopilotTokenMetric(metrics map[string]float64, name string, value *int64) {
	if value != nil {
		metrics[name] += float64(*value)
	}
}

func hasCopilotModelUsage(usage telemetry.ModelUsage) bool {
	return usage.InputTokens != nil || usage.OutputTokens != nil ||
		usage.CacheReadTokens != nil || usage.CacheWriteTokens != nil ||
		usage.ReasoningTokens != nil || usage.CopilotPremiumRequests != nil ||
		usage.NanoAIU != nil
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

func newHarnessSessionID() (string, error) {
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
	var home, userProfile, homeDrive, homePath string
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
		case "USERPROFILE":
			userProfile = value
		case "HOMEDRIVE":
			homeDrive = value
		case "HOMEPATH":
			homePath = value
		}
	}
	switch {
	case home != "":
		return filepath.Join(home, ".copilot"), true
	case userProfile != "":
		return filepath.Join(userProfile, ".copilot"), true
	case homeDrive != "" && homePath != "":
		profileHome := homeDrive
		if os.IsPathSeparator(homePath[0]) {
			profileHome += homePath
		} else {
			profileHome = filepath.Join(homeDrive, homePath)
		}
		return filepath.Join(profileHome, ".copilot"), true
	default:
		return "", false
	}
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
