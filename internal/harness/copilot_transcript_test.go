package harness

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestConvertCopilotSessionEventsFixture(t *testing.T) {
	native := readTestData(t, "copilot-session-events.jsonl")
	want := readTestData(t, "copilot-transcript.jsonl")

	for i := 0; i < 2; i++ {
		got, ok := convertCopilotSessionEvents(bytes.NewReader(native), 0)
		if !ok {
			t.Fatal("convertCopilotSessionEvents did not recognize the native fixture")
		}
		if !bytes.Equal(got.data, want) {
			t.Fatalf("converted transcript:\n%s\nwant:\n%s", got.data, want)
		}
		if got.truncated || got.droppedBytes != 0 {
			t.Fatalf("unexpected truncation: truncated=%v dropped=%d", got.truncated, got.droppedBytes)
		}
	}
}

func TestConvertCopilotSessionEventsRejectsPartialTrailingRecord(t *testing.T) {
	native := append(readTestData(t, "copilot-session-events.jsonl"), []byte("{partial")...)

	if _, ok := convertCopilotSessionEvents(bytes.NewReader(native), 0); ok {
		t.Fatal("convertCopilotSessionEvents accepted a transcript with a partial trailing record")
	}
}

func TestCopilotAdapterPrefersNativeSessionTranscript(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	configHome := filepath.Join(userHome, ".copilot")
	if err := os.MkdirAll(configHome, 0o755); err != nil {
		t.Fatalf("mkdir Copilot config home: %v", err)
	}
	config := []byte(`{"model":"configured-model"}`)
	if err := os.WriteFile(filepath.Join(configHome, "config"), config, 0o600); err != nil {
		t.Fatalf("write Copilot config: %v", err)
	}

	workspace := t.TempDir()
	native := readTestData(t, "copilot-session-events.jsonl")
	want := readTestData(t, "copilot-transcript.jsonl")
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: []byte("stdout compatibility floor"), ExitCode: 0},
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
	if !bytes.Equal(out.Transcript, want) {
		t.Fatalf("Transcript:\n%s\nwant native conversion:\n%s", out.Transcript, want)
	}
	path, ok := nativeSessionLogForRequest(runner.lastReq)
	if !ok {
		t.Fatalf("Copilot command/env missing native session location: %+v", runner.lastReq)
	}
	home := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	if home != configHome {
		t.Fatalf("native session home = %q, want active config home %q", home, configHome)
	}
	gotConfig, err := os.ReadFile(filepath.Join(home, "config"))
	if err != nil {
		t.Fatalf("read active Copilot config: %v", err)
	}
	if !bytes.Equal(gotConfig, config) {
		t.Fatalf("active Copilot config = %q, want %q", gotConfig, config)
	}
}

func TestCopilotAdapterFallsBackWhenNativeSessionLogUnavailable(t *testing.T) {
	oversized := []byte(`{"type":"user.message","data":{"content":"native prefix"}}` + "\n")
	oversized = append(oversized, bytes.Repeat([]byte("x"), int(maxCopilotSessionEventBytes)+1)...)
	oversized = append(oversized, '\n')
	malformedBetweenSupported := []byte(
		`{"type":"user.message","data":{"content":"native prefix"}}` + "\n" +
			"{not json}\n" +
			`{"type":"assistant.message","data":{"messageId":"message-1","content":"native suffix"}}` + "\n",
	)

	for _, tc := range []struct {
		name string
		log  []byte
	}{
		{name: "missing"},
		{name: "unsupported", log: []byte(`{"type":"session.start","data":{}}` + "\n")},
		{name: "malformed", log: []byte("{not json\n")},
		{name: "malformed between supported", log: malformedBetweenSupported},
		{name: "oversized after supported", log: oversized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
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

func TestCopilotAdapterBoundsNativeSessionTranscript(t *testing.T) {
	const limit = int64(80)
	workspace := t.TempDir()
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
	if !strings.Contains(string(out.Transcript), "[transcript truncated:") {
		t.Fatalf("Transcript missing truncation marker: %q", out.Transcript)
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
	path, ok := nativeSessionLogForRequest(req)
	if !ok {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func nativeSessionLogForRequest(req ProcessRequest) (string, bool) {
	home, ok := copilotConfigHome(req.Env)
	if !ok {
		return "", false
	}
	for i, arg := range req.Command {
		switch {
		case arg == "--session-id" && i+1 < len(req.Command):
			return copilotSessionLogPath(home, req.Command[i+1]), true
		case strings.HasPrefix(arg, "--session-id="):
			return copilotSessionLogPath(home, strings.TrimPrefix(arg, "--session-id=")), true
		}
	}
	return "", false
}
