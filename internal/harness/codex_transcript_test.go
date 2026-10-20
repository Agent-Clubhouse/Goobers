package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
)

// Fixtures follow openai/codex exec_events.rs at
// e18a20d6eb6f5ede32a69fb56e3fe276d5e9d914. The exec processor uses cumulative
// usage.total, including on resume; repeated terminal snapshots are deliberate.
func TestCodexTranscriptNormalizesToolsAndResumeUsage(t *testing.T) {
	workspace := t.TempDir()
	runner := &codexSequenceRunner{results: []ProcessResult{
		{Transcript: readTestData(t, "codex-events-initial.jsonl")},
		{Transcript: readTestData(t, "codex-events-resumed.jsonl")},
	}}
	adapter := &CodexAdapter{Command: []string{"codex"}, Runner: runner, ModelCredential: func(context.Context) (string, error) { return "sk-fixture", nil }}
	out, err := adapter.Run(context.Background(), RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace, CompletionPath: DefaultResultPath, Model: "gpt-5-codex"})
	if err != nil {
		t.Fatal(err)
	}
	if out.TranscriptSchema != telemetry.GenAIEventSchema {
		t.Fatalf("schema=%q", out.TranscriptSchema)
	}
	expected := map[string]float64{telemetry.AttrGenAIUsageInputTokens: 120, telemetry.AttrGenAIUsageOutputTokens: 12, telemetry.AttrUsageCacheReadTokens: 60, telemetry.AttrUsageCacheWriteTokens: 4, telemetry.AttrUsageReasoningTokens: 3}
	for name, want := range expected {
		if out.Metrics[name] != want {
			t.Fatalf("metric %s=%v want %v", name, out.Metrics[name], want)
		}
	}
	if _, ok := out.Metrics[telemetry.AttrUsageCostUSD]; ok {
		t.Fatal("invented vendor cost")
	}
	calls, results := map[string]int{}, map[string]int{}
	var usage *transcriptUsage
	var usageRecords, prompts int
	var initial, repair, unknown bool
	for _, event := range decodeTranscriptEvents(t, out.Transcript) {
		if event.Role == "user" {
			prompts++
		}
		initial = initial || event.Content == "Initial response without completion envelope."
		repair = repair || event.Content == "Repaired completion."
		unknown = unknown || strings.Contains(event.Content, `"new_field":"preserve-this-evidence"`)
		if event.Usage != nil {
			usageRecords++
			usage = event.Usage
		}
		if event.ToolCall == nil {
			continue
		}
		if event.Role == "assistant" {
			calls[event.ToolCall.ID]++
			if len(event.ToolCall.Arguments) == 0 {
				t.Fatal("tool arguments missing")
			}
		}
		if event.Role == "tool" {
			results[event.ToolCall.ID]++
			switch event.ToolCall.ID {
			case "codex-1:item_1", "codex-1:item_3":
				if event.ToolCall.Success == nil || !*event.ToolCall.Success {
					t.Fatalf("expected successful tool: %+v", event)
				}
			case "codex-1:item_2", "codex-2:item_1":
				if event.ToolCall.Success == nil || *event.ToolCall.Success {
					t.Fatalf("expected failed tool: %+v", event)
				}
			case "codex-1:item_4":
				if event.ToolCall.Success != nil {
					t.Fatal("web-search success invented")
				}
			}
		}
	}
	if len(calls) != 5 || len(results) != 5 {
		t.Fatalf("calls=%v results=%v", calls, results)
	}
	for id, count := range calls {
		if count != 1 || results[id] != 1 {
			t.Fatalf("duplicate/unmatched tool %s", id)
		}
	}
	if prompts != 2 || !initial || !repair || !unknown {
		t.Fatalf("missing transcript evidence: %s", out.Transcript)
	}
	if usageRecords != 1 || usage.InputTokens == nil || *usage.InputTokens != 120 || usage.CacheReadTokens == nil || *usage.CacheReadTokens != 60 || usage.ReasoningTokens == nil || *usage.ReasoningTokens != 3 || usage.Cost != nil {
		t.Fatalf("usage records=%d usage=%+v", usageRecords, usage)
	}
	assertAdapterLifecycle(t, out, "codex")
}

func TestCodexTranscriptKeepsTerminalUsageAfterOutputTruncation(t *testing.T) {
	workspace := t.TempDir()
	stream := fmt.Sprintf("{\"type\":\"item.completed\",\"item\":{\"id\":\"large\",\"type\":\"agent_message\",\"text\":%q}}\n", strings.Repeat("large output ", 2000)) + `{"type":"turn.completed","usage":{"input_tokens":25,"cached_input_tokens":0,"output_tokens":4}}` + "\n"
	runner := launcherProcessRunner(func(_ context.Context, req ProcessRequest) (ProcessResult, error) {
		// Exercise chunked stdout independent of the retained combined stream.
		for offset := 0; offset < len(stream); offset += 73 {
			end := min(offset+73, len(stream))
			_, _ = req.StdoutCapture.Write([]byte(stream[offset:end]))
		}
		err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		return ProcessResult{Transcript: []byte(stream[:512]), TranscriptTruncated: true, TranscriptDroppedBytes: int64(len(stream) - 512)}, err
	})
	adapter := &CodexAdapter{Command: []string{"codex"}, Runner: runner, ModelCredential: func(context.Context) (string, error) { return "sk-fixture", nil }}
	out, err := adapter.Run(context.Background(), RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace, CompletionPath: DefaultResultPath, MaxTranscriptBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	if !out.TranscriptTruncated || out.TranscriptDroppedBytes == 0 {
		t.Fatal("truncation evidence lost")
	}
	if out.Metrics[telemetry.AttrGenAIUsageInputTokens] != 25 {
		t.Fatal("terminal metrics lost")
	}
	var found bool
	for _, event := range decodeTranscriptEvents(t, out.Transcript) {
		if event.Usage != nil {
			found = true
			if event.Usage.CacheReadTokens == nil || *event.Usage.CacheReadTokens != 0 {
				t.Fatal("measured zero became unknown")
			}
		}
	}
	if !found {
		t.Fatal("terminal canonical usage lost")
	}
}

func TestCodexTranscriptPreservesObservedUsageOnProcessError(t *testing.T) {
	workspace := t.TempDir()
	runner := launcherProcessRunner(func(_ context.Context, req ProcessRequest) (ProcessResult, error) {
		stream := readTestData(t, "codex-events-initial.jsonl")
		_, _ = req.StdoutCapture.Write(stream)
		return ProcessResult{Transcript: stream, ExitCode: 1}, errors.New("fixture process failure")
	})
	adapter := &CodexAdapter{Command: []string{"codex"}, Runner: runner, ModelCredential: func(context.Context) (string, error) { return "sk-fixture", nil }}
	out, err := adapter.Run(context.Background(), RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace, CompletionPath: DefaultResultPath})
	if err == nil {
		t.Fatal("process failure swallowed")
	}
	if out.Metrics[telemetry.AttrUsageCacheReadTokens] != 50 {
		t.Fatal("observed usage discarded on process error")
	}
	if !strings.Contains(string(out.Transcript), "lookup unavailable") {
		t.Fatal("tool failure evidence lost")
	}
}

func TestCodexTranscriptMissingUsageStaysUnknown(t *testing.T) {
	for _, stream := range []string{`{"type":"turn.completed"}`, `{"type":"turn.completed","usage":{}}`, `{"type":"turn.failed","error":{"message":"failed before usage"}}`} {
		parsed, err := parseCodexJSONL([]byte(stream))
		if err != nil {
			t.Fatal(err)
		}
		transcript := newCodexTranscript(4096, "")
		transcript.beginTurn("fixture prompt")
		transcript.observe([]byte(stream))
		result, err := transcript.finish(parsed.metrics, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range decodeTranscriptEvents(t, result.data) {
			if event.Usage != nil {
				t.Fatalf("unknown usage became zero: %+v", event.Usage)
			}
		}
	}
}
