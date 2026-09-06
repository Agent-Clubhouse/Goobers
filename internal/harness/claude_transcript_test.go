package harness

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/telemetry"
)

func TestConvertClaudeStreamCapturesToolsTranscriptAndUsage(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-sonnet-4-6"}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"go test ./..."}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"ok","is_error":false}]}}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"finished"}]}}`,
		`{"type":"result","subtype":"success","result":"finished","total_cost_usd":0.42,"usage":{"input_tokens":100,"output_tokens":20},"modelUsage":{"claude-opus-4-6":{"inputTokens":30,"outputTokens":5,"cacheReadInputTokens":7,"cacheCreationInputTokens":3,"costUSD":0.2},"claude-sonnet-4-6":{"inputTokens":70,"outputTokens":15,"cacheReadInputTokens":11,"cacheCreationInputTokens":4,"costUSD":0.22}}}`,
	}, "\n")
	capture, ok := convertClaudeStreams([]io.Reader{strings.NewReader(stream)}, []string{"implement the task"}, 1<<20, 0)
	if !ok {
		t.Fatal("convertClaudeStreams returned false")
	}
	if got := capture.metrics[telemetry.AttrGenAIUsageInputTokens]; got != 100 {
		t.Fatalf("input tokens = %v, want 100", got)
	}
	if got := capture.metrics[telemetry.AttrGenAIUsageOutputTokens]; got != 20 {
		t.Fatalf("output tokens = %v, want 20", got)
	}
	if got := capture.metrics[telemetry.AttrUsageCostUSD]; got != 0.42 {
		t.Fatalf("cost = %v, want 0.42", got)
	}
	if got := capture.metrics[telemetry.AttrUsageNanoAIU]; got != 42_000_000_000 {
		t.Fatalf("normalized nano-AIU = %v, want 42000000000", got)
	}
	if got := capture.metrics[telemetry.AttrUsageCacheReadTokens]; got != 18 {
		t.Fatalf("cache-read tokens = %v, want 18", got)
	}
	if got := capture.metrics[telemetry.AttrUsageCacheWriteTokens]; got != 7 {
		t.Fatalf("cache-write tokens = %v, want 7", got)
	}
	if len(capture.modelUsage) != 2 ||
		capture.modelUsage[0].Model != "claude-opus-4-6" ||
		capture.modelUsage[0].CostUSD == nil || *capture.modelUsage[0].CostUSD != 0.2 ||
		capture.modelUsage[0].NanoAIU == nil || *capture.modelUsage[0].NanoAIU != 20_000_000_000 ||
		capture.modelUsage[0].CostBasis != telemetry.CostBasisVendorReported ||
		capture.modelUsage[1].Model != "claude-sonnet-4-6" {
		t.Fatalf("model usage = %+v", capture.modelUsage)
	}

	lines := bytes.Split(bytes.TrimSpace(capture.data), []byte("\n"))
	var sawPrompt, sawToolStart, sawToolResult, sawFinal bool
	for _, line := range lines {
		var event transcriptEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode canonical event: %v\n%s", err, line)
		}
		switch {
		case event.Role == "user" && event.Content == "implement the task":
			sawPrompt = true
		case event.Role == "assistant" && event.ToolCall != nil &&
			event.ToolCall.ID == "tool-1" && event.ToolCall.Name == "Bash":
			sawToolStart = true
		case event.Role == "tool" && event.ToolCall != nil &&
			event.ToolCall.ID == "tool-1" && event.ToolCall.Success != nil && *event.ToolCall.Success:
			sawToolResult = true
		case event.Role == "assistant" && event.Content == "finished":
			sawFinal = true
		}
	}
	if !sawPrompt || !sawToolStart || !sawToolResult || !sawFinal {
		t.Fatalf("canonical transcript missing expected events:\n%s", capture.data)
	}
}

func TestConvertClaudeStreamIgnoresDuplicateAndResetAssistantTotals(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"id":"msg-1","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":10},"content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{}}]}}`,
		`{"type":"assistant","message":{"id":"msg-1","model":"claude-sonnet-4-6","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":10},"content":[{"type":"tool_use","id":"tool-2","name":"Read","input":{}}]}}`,
		`{"type":"assistant","message":{"id":"msg-2","model":"claude-sonnet-4-6","usage":{"input_tokens":5,"output_tokens":1},"content":[{"type":"text","text":"after clear"}]}}`,
		`{"type":"result","subtype":"success","result":"finished","total_cost_usd":0.03,"modelUsage":{"claude-sonnet-4-6":{"inputTokens":12,"outputTokens":3,"cacheReadInputTokens":2,"cacheCreationInputTokens":1,"costUSD":0.03}}}`,
	}, "\n")
	capture, ok := convertClaudeStreams([]io.Reader{strings.NewReader(stream)}, nil, 1<<20, 0)
	if !ok {
		t.Fatal("convertClaudeStreams returned false")
	}
	want := map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  12,
		telemetry.AttrGenAIUsageOutputTokens: 3,
		telemetry.AttrUsageCacheReadTokens:   2,
		telemetry.AttrUsageCacheWriteTokens:  1,
		telemetry.AttrUsageCostUSD:           0.03,
		telemetry.AttrUsageNanoAIU:           3_000_000_000,
	}
	if !mapsEqual(capture.metrics, want) {
		t.Fatalf("usage = %#v, want result-envelope totals %#v", capture.metrics, want)
	}
}

func TestConvertClaudeStreamUsesWholeAgentTreeUsage(t *testing.T) {
	stream := `{"type":"result","subtype":"success","result":"finished","usage":{"input_tokens":10,"output_tokens":2},"modelUsage":{"claude-opus-4-6":{"inputTokens":30,"outputTokens":5},"claude-sonnet-4-6":{"inputTokens":70,"outputTokens":15}}}`
	capture, ok := convertClaudeStreams([]io.Reader{strings.NewReader(stream)}, nil, 1<<20, 0)
	if !ok {
		t.Fatal("convertClaudeStreams returned false")
	}
	if got := capture.metrics[telemetry.AttrGenAIUsageInputTokens]; got != 100 {
		t.Fatalf("input tokens = %v, want 100", got)
	}
	if got := capture.metrics[telemetry.AttrGenAIUsageOutputTokens]; got != 20 {
		t.Fatalf("output tokens = %v, want 20", got)
	}
}

func TestConvertClaudeStreamFallsBackToTopLevelUsage(t *testing.T) {
	stream := `{"type":"result","subtype":"success","result":"finished","usage":{"input_tokens":10,"output_tokens":2}}`
	capture, ok := convertClaudeStreams([]io.Reader{strings.NewReader(stream)}, nil, 1<<20, 0)
	if !ok {
		t.Fatal("convertClaudeStreams returned false")
	}
	if got := capture.metrics[telemetry.AttrGenAIUsageInputTokens]; got != 10 {
		t.Fatalf("input tokens = %v, want 10", got)
	}
	if got := capture.metrics[telemetry.AttrGenAIUsageOutputTokens]; got != 2 {
		t.Fatalf("output tokens = %v, want 2", got)
	}
}

func TestConvertClaudeStreamBoundsCanonicalTranscript(t *testing.T) {
	longText := strings.Repeat("x", 2000)
	stream := `{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"` +
		longText + `"}]}}` + "\n" +
		`{"type":"result","subtype":"success","result":"` + longText + `","usage":{"input_tokens":1,"output_tokens":2}}`
	capture, ok := convertClaudeStreams([]io.Reader{strings.NewReader(stream)}, []string{longText}, 512, 17)
	if !ok {
		t.Fatal("convertClaudeStreams returned false")
	}
	if !capture.truncated || capture.droppedBytes <= 17 {
		t.Fatalf("truncation = %v, dropped = %d", capture.truncated, capture.droppedBytes)
	}
	if !bytes.Contains(capture.data, []byte(`"truncated":true`)) ||
		!bytes.Contains(capture.data, []byte("transcript truncated")) {
		t.Fatalf("bounded transcript lacks truncation record:\n%s", capture.data)
	}
}

func TestConvertClaudeStreamCapturesFailedResultErrors(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-sonnet-4-6"}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"` + strings.Repeat("x", 1000) + `"}]}}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["API Error: overloaded","Request ID: req-123"],"usage":{"input_tokens":4,"output_tokens":0}}`,
	}, "\n")
	capture, ok := convertClaudeStreams([]io.Reader{strings.NewReader(stream)}, []string{"implement the task"}, 512, 0)
	if !ok {
		t.Fatal("convertClaudeStreams returned false")
	}
	if !capture.truncated {
		t.Fatal("expected canonical transcript truncation")
	}

	var diagnostics []string
	for _, line := range bytes.Split(bytes.TrimSpace(capture.data), []byte("\n")) {
		var event transcriptEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode canonical event: %v\n%s", err, line)
		}
		if event.Role == "system" && event.Content != "" && !event.Truncated {
			diagnostics = append(diagnostics, event.Content)
		}
	}
	if got, want := strings.Join(diagnostics, "\n"), "API Error: overloaded\nRequest ID: req-123"; got != want {
		t.Fatalf("failed-result diagnostics = %q, want %q\n%s", got, want, capture.data)
	}
}

func TestConvertClaudeStreamRejectsNonNativeOutput(t *testing.T) {
	if _, ok := convertClaudeStreams([]io.Reader{strings.NewReader("ordinary stdout\nnot json\n")}, nil, 1024, 0); ok {
		t.Fatal("non-native output was accepted as a Claude stream")
	}
}
