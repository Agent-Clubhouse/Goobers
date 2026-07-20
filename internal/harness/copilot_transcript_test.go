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
		})
	}
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
	const limit = int64(80)
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	native := readTestData(t, "copilot-session-events.jsonl")
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
