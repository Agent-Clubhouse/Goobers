package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestConvertCopilotSessionEventsFixture(t *testing.T) {
	native := readTestData(t, "copilot-session-events.jsonl")
	want := canonicalTranscriptFixture(t, readTestData(t, "copilot-transcript.jsonl"))

	for i := 0; i < 2; i++ {
		got, ok := convertCopilotSessionEvents(bytes.NewReader(native), 0)
		if !ok {
			t.Fatal("convertCopilotSessionEvents did not recognize the native fixture")
		}
		if !bytes.Equal(got.data, want) {
			t.Fatalf("converted transcript:\n%s\nwant:\n%s", got.data, want)
		}
		if events := decodeTranscriptEvents(t, got.data); len(events) != 9 {
			t.Fatalf("converted transcript events = %d, want 9", len(events))
		}
		if got.truncated || got.droppedBytes != 0 {
			t.Fatalf("unexpected truncation: truncated=%v dropped=%d", got.truncated, got.droppedBytes)
		}
		wantMetrics := map[string]float64{
			telemetry.AttrGenAIUsageInputTokens:  28,
			telemetry.AttrGenAIUsageOutputTokens: 13,
			telemetry.AttrCopilotPremiumRequests: 2,
			telemetry.AttrUsageCostUSD:           0.00000003,
		}
		if !mapsEqual(got.metrics, wantMetrics) {
			t.Fatalf("usage metrics = %#v, want %#v", got.metrics, wantMetrics)
		}
		if len(got.modelUsage) != 2 {
			t.Fatalf("model usage = %#v, want two models", got.modelUsage)
		}
		first, second := got.modelUsage[0], got.modelUsage[1]
		if first.Model != "a-model" ||
			first.InputTokens == nil || *first.InputTokens != 8 ||
			first.OutputTokens == nil || *first.OutputTokens != 3 ||
			first.CopilotPremiumRequests == nil || *first.CopilotPremiumRequests != 0.5 ||
			first.CostUSD == nil || *first.CostUSD != 0.00000001 {
			t.Fatalf("a-model usage = %#v", first)
		}
		if second.Model != "z-model" ||
			second.InputTokens == nil || *second.InputTokens != 20 ||
			second.OutputTokens == nil || *second.OutputTokens != 10 ||
			second.CopilotPremiumRequests == nil || *second.CopilotPremiumRequests != 1.5 ||
			second.CostUSD == nil || *second.CostUSD != 0.00000002 {
			t.Fatalf("z-model usage = %#v", second)
		}
	}
}

func TestCopilotUsagePreservesAbsentAndZero(t *testing.T) {
	absent, ok := convertCopilotSessionEvents(strings.NewReader(
		`{"type":"session.shutdown","data":{"modelMetrics":{"model":{"requests":{"count":1},"usage":{}}}}}`+"\n",
	), 0)
	if !ok {
		t.Fatal("shutdown event was not recognized")
	}
	if absent.metrics != nil {
		t.Fatalf("absent usage produced metrics: %#v", absent.metrics)
	}
	if absent.modelUsage != nil {
		t.Fatalf("absent usage produced model metrics: %#v", absent.modelUsage)
	}

	zero, ok := convertCopilotSessionEvents(strings.NewReader(
		`{"type":"session.shutdown","data":{"modelMetrics":{"model":{"requests":{"count":1,"cost":0},"usage":{"inputTokens":0,"outputTokens":0},"totalNanoAiu":0}}}}`+"\n",
	), 0)
	if !ok {
		t.Fatal("zero-valued shutdown event was not recognized")
	}
	for _, name := range []string{
		telemetry.AttrGenAIUsageInputTokens,
		telemetry.AttrGenAIUsageOutputTokens,
		telemetry.AttrCopilotPremiumRequests,
		telemetry.AttrUsageCostUSD,
	} {
		value, present := zero.metrics[name]
		if !present || value != 0 {
			t.Errorf("zero metric %q = %v, present=%v", name, value, present)
		}
	}
	if len(zero.modelUsage) != 1 ||
		zero.modelUsage[0].InputTokens == nil || *zero.modelUsage[0].InputTokens != 0 ||
		zero.modelUsage[0].OutputTokens == nil || *zero.modelUsage[0].OutputTokens != 0 ||
		zero.modelUsage[0].CopilotPremiumRequests == nil || *zero.modelUsage[0].CopilotPremiumRequests != 0 ||
		zero.modelUsage[0].CostUSD == nil || *zero.modelUsage[0].CostUSD != 0 {
		t.Fatalf("zero model usage = %#v", zero.modelUsage)
	}
}

func TestCopilotUsageMatchesEnvelopeAndSpan(t *testing.T) {
	native := readTestData(t, "copilot-session-events.jsonl")
	want := map[string]float64{
		telemetry.AttrGenAIUsageInputTokens:  28,
		telemetry.AttrGenAIUsageOutputTokens: 13,
		telemetry.AttrCopilotPremiumRequests: 2,
		telemetry.AttrUsageCostUSD:           0.00000003,
	}

	for _, tc := range []struct {
		name          string
		native        []byte
		want          map[string]float64
		wantModels    string
		wantSpanModel string
	}{
		{name: "known session fixture", native: native, want: want, wantModels: "a-model,z-model"},
		{
			name:   "single model",
			native: []byte(`{"type":"session.shutdown","data":{"modelMetrics":{"gpt-5.4":{"requests":{"cost":0.5},"usage":{"inputTokens":1,"outputTokens":2},"totalNanoAiu":1000}}}}` + "\n"),
			want: map[string]float64{
				telemetry.AttrGenAIUsageInputTokens:  1,
				telemetry.AttrGenAIUsageOutputTokens: 2,
				telemetry.AttrCopilotPremiumRequests: 0.5,
				telemetry.AttrUsageCostUSD:           0.00000001,
			},
			wantModels:    "gpt-5.4",
			wantSpanModel: "gpt-5.4",
		},
		{name: "usage unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			runner := &fakeProcessRunner{
				result: ProcessResult{Transcript: []byte("stdout compatibility floor"), ExitCode: 0},
				act: func(req ProcessRequest) error {
					if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
						return err
					}
					if tc.native != nil {
						return writeNativeSessionLog(req, tc.native)
					}
					return nil
				},
			}
			recorder := &fakeRecorder{}
			executor, err := NewExecutor(
				&CopilotAdapter{Command: []string{"copilot"}, Runner: runner},
				testInjector(t, "", "", noopRegistrar{}),
				recorder,
				recorder,
				recorder,
				journal.NewPatternScrubber(),
				"",
			)
			if err != nil {
				t.Fatalf("NewExecutor: %v", err)
			}

			exporter := telemetry.NewMemoryExporter()
			client, err := telemetry.New(context.Background(), telemetry.Config{
				ServiceName:  "copilot-usage-test",
				SpanExporter: exporter,
			})
			if err != nil {
				t.Fatalf("telemetry.New: %v", err)
			}
			t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

			const runID = "0123456789abcdef0123456789abcdef"
			ctx, span, err := client.StartTask(context.Background(), telemetry.TaskAttributes{
				Gaggle:     "example",
				WorkflowID: "default-implement",
				RunID:      runID,
				TaskID:     "implement",
				TaskType:   telemetry.StageTypeAgentic,
			})
			if err != nil {
				t.Fatalf("StartTask: %v", err)
			}
			env := testEnvelope(workspace, "repo:read")
			env.RunID = runID
			result, err := executor.Invoke(ctx, env)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			span.End()

			if !mapsEqual(result.Metrics, tc.want) {
				t.Fatalf("envelope metrics = %#v, want %#v", result.Metrics, tc.want)
			}
			spans := exporter.Spans()
			if len(spans) != 1 {
				t.Fatalf("exported spans = %d, want 1", len(spans))
			}
			gotSpan := make(map[string]float64, 4)
			var spanModel string
			for _, attr := range spans[0].Attributes() {
				switch string(attr.Key) {
				case telemetry.AttrGenAIUsageInputTokens, telemetry.AttrGenAIUsageOutputTokens:
					gotSpan[string(attr.Key)] = float64(attr.Value.AsInt64())
				case telemetry.AttrCopilotPremiumRequests, telemetry.AttrUsageCostUSD:
					gotSpan[string(attr.Key)] = attr.Value.AsFloat64()
				case telemetry.AttrGenAIResponseModel:
					spanModel = attr.Value.AsString()
				}
			}
			if !mapsEqual(gotSpan, tc.want) {
				t.Fatalf("span metrics = %#v, want %#v", gotSpan, tc.want)
			}
			if spanModel != tc.wantSpanModel {
				t.Fatalf("span model = %q, want %q", spanModel, tc.wantSpanModel)
			}
			var models []string
			for _, event := range spans[0].Events() {
				if event.Name != telemetry.GenAIModelUsageEventName {
					continue
				}
				for _, attr := range event.Attributes {
					if string(attr.Key) == telemetry.AttrGenAIResponseModel {
						models = append(models, attr.Value.AsString())
					}
				}
			}
			if strings.Join(models, ",") != tc.wantModels {
				t.Fatalf("span model usage = %q, want %q", strings.Join(models, ","), tc.wantModels)
			}
		})
	}
}

func TestConvertCopilotSessionEventsSalvagesPartialTrailingRecord(t *testing.T) {
	native := append(readTestData(t, "copilot-session-events.jsonl"), []byte("{partial")...)
	want := canonicalTranscriptFixture(t, readTestData(t, "copilot-transcript.jsonl"))

	got, ok := convertCopilotSessionEvents(bytes.NewReader(native), 0)
	if !ok {
		t.Fatal("convertCopilotSessionEvents discarded valid events before a partial trailing record")
	}
	if !bytes.Equal(got.data, want) {
		t.Fatalf("converted transcript:\n%s\nwant salvaged events:\n%s", got.data, want)
	}
}

func TestCopilotAdapterPrefersNativeSessionTranscript(t *testing.T) {
	workspace := t.TempDir()
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	copilotHome := filepath.Join(userHome, ".copilot")
	if err := os.MkdirAll(copilotHome, 0o700); err != nil {
		t.Fatalf("prepare Copilot home: %v", err)
	}
	const config = `{"model":"configured-model"}`
	if err := os.WriteFile(filepath.Join(copilotHome, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatalf("write Copilot config: %v", err)
	}
	native := readTestData(t, "copilot-session-events.jsonl")
	want := canonicalTranscriptFixture(t, readTestData(t, "copilot-transcript.jsonl"))
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: []byte("stdout compatibility floor"), ExitCode: 0},
		act: func(req ProcessRequest) error {
			for _, entry := range req.Env {
				if strings.HasPrefix(entry, "COPILOT_HOME=") {
					return errors.New("COPILOT_HOME unexpectedly replaced active Copilot configuration")
				}
			}
			gotHome, ok := copilotConfigHome(req.Env)
			if !ok || gotHome != copilotHome {
				return fmt.Errorf("Copilot home = %q, %v; want %q", gotHome, ok, copilotHome)
			}
			gotConfig, err := os.ReadFile(filepath.Join(gotHome, "config.json"))
			if err != nil {
				return fmt.Errorf("read active Copilot config: %w", err)
			}
			if string(gotConfig) != config {
				return fmt.Errorf("active Copilot config = %q, want %q", gotConfig, config)
			}
			if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
				return err
			}
			return writeNativeSessionLog(req, native)
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "unused", "unused"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(out.Transcript, want) {
		t.Fatalf("Transcript:\n%s\nwant native conversion:\n%s", out.Transcript, want)
	}
	if out.TranscriptSchema != telemetry.GenAIEventSchema {
		t.Fatalf("TranscriptSchema = %q, want %q", out.TranscriptSchema, telemetry.GenAIEventSchema)
	}
	path, ok := nativeSessionLogPath(runner.lastReq)
	if !ok {
		t.Fatalf("Copilot command/env missing native session location: %+v", runner.lastReq)
	}
	if filepath.Dir(filepath.Dir(filepath.Dir(path))) != copilotHome {
		t.Fatalf("session log path %q is not rooted under active Copilot home %q", path, copilotHome)
	}
}

func TestCopilotAdapterKeepsStdoutWhenNativeLogOnlyHasUsage(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	floor := []byte("stdout compatibility floor")
	native := []byte(`{"type":"session.shutdown","data":{"totalPremiumRequests":2,"totalNanoAiu":3000,"modelMetrics":{}}}` + "\n")
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: floor, ExitCode: 0},
		act: func(req ProcessRequest) error {
			if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
				return err
			}
			return writeNativeSessionLog(req, native)
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "unused", "unused"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(out.Transcript, floor) {
		t.Fatalf("Transcript = %q, want floor %q", out.Transcript, floor)
	}
	wantMetrics := map[string]float64{
		telemetry.AttrCopilotPremiumRequests: 2,
		telemetry.AttrUsageCostUSD:           0.00000003,
	}
	if !mapsEqual(out.Metrics, wantMetrics) {
		t.Fatalf("usage metrics = %#v, want %#v", out.Metrics, wantMetrics)
	}
}

func TestCopilotAdapterFallsBackWhenNativeSessionLogUnavailable(t *testing.T) {
	oversized := []byte(`{"type":"user.message","data":{"content":"native prefix"}}` + "\n")
	oversized = append(oversized, bytes.Repeat([]byte("x"), int(maxCopilotSessionEventBytes)+1)...)
	oversized = append(oversized, '\n')
	malformedBetweenValid := []byte(
		`{"type":"user.message","data":{"content":"native prefix"}}` + "\n" +
			"{not json}\n" +
			`{"type":"assistant.message","data":{"messageId":"message-1","content":"native suffix"}}`,
	)

	for _, tc := range []struct {
		name string
		log  []byte
	}{
		{name: "missing"},
		{name: "unsupported", log: []byte(`{"type":"session.start","data":{}}` + "\n")},
		{name: "malformed", log: []byte("{not json\n")},
		{name: "malformed between supported", log: malformedBetweenValid},
		{name: "oversized after supported", log: oversized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			floor := []byte("stdout compatibility floor")
			runner := &fakeProcessRunner{
				result: ProcessResult{Transcript: floor, ExitCode: 0},
				act: func(req ProcessRequest) error {
					if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
						return err
					}
					if tc.log != nil {
						return writeNativeSessionLog(req, tc.log)
					}
					return nil
				},
			}
			adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

			out, err := adapter.Run(context.Background(), RunRequest{
				Envelope:       testEnvelope(workspace),
				Workspace:      workspace,
				CompletionPath: DefaultResultPath,
				Credentials:    pushCredentials(t, "unused", "unused"),
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !bytes.Equal(out.Transcript, floor) {
				t.Fatalf("Transcript = %q, want floor %q", out.Transcript, floor)
			}
			if out.Metrics != nil {
				t.Fatalf("unavailable native usage produced metrics: %#v", out.Metrics)
			}
		})
	}
}

func mapsEqual(got, want map[string]float64) bool {
	if len(got) != len(want) {
		return false
	}
	for name, value := range want {
		if gotValue, ok := got[name]; !ok || gotValue != value {
			return false
		}
	}
	return true
}

func TestCopilotAdapterSkipsNativeTranscriptForSelectedSession(t *testing.T) {
	native := readTestData(t, "copilot-session-events.jsonl")
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	floor := []byte("stdout compatibility floor")
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: floor, ExitCode: 0},
		act: func(req ProcessRequest) error {
			if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
				return err
			}
			return writeNativeSessionLog(req, native)
		},
	}
	adapter := &CopilotAdapter{
		Command:   []string{"copilot"},
		ExtraArgs: []string{"--session-id", "existing-session"},
		Runner:    runner,
	}

	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "unused", "unused"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sessionID, ok := nativeSessionID(runner.lastReq); !ok || sessionID != "existing-session" {
		t.Fatalf("selected-session command changed session ID: %+v", runner.lastReq.Command)
	}
	if !bytes.Equal(out.Transcript, floor) {
		t.Fatalf("Transcript = %q, want floor %q", out.Transcript, floor)
	}
}

func TestCopilotAdapterBoundsNativeSessionTranscript(t *testing.T) {
	const limit = int64(512)
	const prompt = "Implement the parser."
	const finalOutput = "The parser is implemented."
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	native := []byte(
		`{"type":"user.message","data":{"content":"` + prompt + `"}}` + "\n" +
			`{"type":"session.error","data":{"message":"` + strings.Repeat("x", int(limit)) + `"}}` + "\n" +
			`{"type":"assistant.message","data":{"messageId":"message-final","model":"gpt-5.4","content":"` + finalOutput + `"}}` + "\n",
	)
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: []byte("floor"), ExitCode: 0},
		act: func(req ProcessRequest) error {
			if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
				return err
			}
			return writeNativeSessionLog(req, native)
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:           testEnvelope(workspace),
		Workspace:          workspace,
		CompletionPath:     DefaultResultPath,
		Credentials:        pushCredentials(t, "unused", "unused"),
		MaxTranscriptBytes: limit,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.TranscriptTruncated || out.TranscriptDroppedBytes <= 0 {
		t.Fatalf("native truncation = %v, dropped = %d; want a positive truncation", out.TranscriptTruncated, out.TranscriptDroppedBytes)
	}
	events := decodeTranscriptEvents(t, out.Transcript)
	if len(events) != 3 {
		t.Fatalf("native transcript events = %#v, want prompt, final output, and marker", events)
	}
	if events[0].Role != "user" || events[0].Content != prompt {
		t.Fatalf("prompt event = %#v, want retained native prompt", events[0])
	}
	if events[1].Role != "assistant" || events[1].Content != finalOutput || !events[1].Truncated {
		t.Fatalf("final output event = %#v, want retained truncated native final output", events[1])
	}
	marker := events[len(events)-1]
	if marker.Role != "system" || !marker.Truncated || !strings.Contains(marker.Content, "[transcript truncated:") {
		t.Fatalf("truncation marker = %#v", marker)
	}
	encodedMarker, err := marshalTranscriptEvents(marker)
	if err != nil {
		t.Fatalf("marshal truncation marker: %v", err)
	}
	if retained := len(out.Transcript) - len(encodedMarker); retained > int(limit) {
		t.Fatalf("retained transcript bytes = %d, want at most %d", retained, limit)
	}
}

func TestCopilotNativeTranscriptUsesExecutorRedaction(t *testing.T) {
	registry, scrubber := journal.DefaultScrubber()
	const secret = "opaque-native-log-secret"
	registry.Register([]byte(secret))
	if got := journal.NewPatternScrubber().Scrub([]byte(secret)); string(got) != secret {
		t.Fatalf("test secret %q is pattern-visible as %q", secret, got)
	}

	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: []byte("floor"), ExitCode: 0},
		act: func(req ProcessRequest) error {
			if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
				return err
			}
			line := []byte(`{"type":"assistant.message","data":{"messageId":"message-1","content":"token=` + secret + `"}}` + "\n")
			return writeNativeSessionLog(req, line)
		},
	}
	recorder := &fakeRecorder{}
	executor, err := NewExecutor(
		&CopilotAdapter{Command: []string{"copilot"}, Runner: runner},
		testInjector(t, "REPO_TOKEN_ENV", secret, registry),
		recorder,
		recorder,
		recorder,
		scrubber,
		"",
	)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	if _, err := executor.Invoke(context.Background(), testEnvelope(workspace, "repo:read")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(recorder.spans) != 1 {
		t.Fatalf("recorded spans = %d, want 1", len(recorder.spans))
	}
	got := string(recorder.spans[0].data)
	if strings.Contains(got, secret) || !strings.Contains(got, journal.Redacted) {
		t.Fatalf("recorded native transcript was not scrubbed: %q", got)
	}
}

func readTestData(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return data
}

func writeNativeSessionLog(req ProcessRequest, data []byte) error {
	path, ok := nativeSessionLogPath(req)
	if !ok {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func nativeSessionLogPath(req ProcessRequest) (string, bool) {
	home, ok := copilotConfigHome(req.Env)
	if !ok {
		return "", false
	}
	sessionID, ok := nativeSessionID(req)
	if !ok {
		return "", false
	}
	return copilotSessionLogPath(home, sessionID), true
}

func nativeSessionID(req ProcessRequest) (string, bool) {
	for i, arg := range req.Command {
		switch {
		case arg == "--session-id" && i+1 < len(req.Command):
			return req.Command[i+1], true
		case strings.HasPrefix(arg, "--session-id="):
			return strings.TrimPrefix(arg, "--session-id="), true
		}
	}
	return "", false
}
