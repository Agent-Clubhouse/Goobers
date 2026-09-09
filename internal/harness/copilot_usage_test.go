package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestCopilotUsageVersionNegotiation(t *testing.T) {
	for _, reported := range []string{"GitHub Copilot CLI 1.0.81-1.", "GitHub Copilot CLI 1.0.81.", "GitHub Copilot CLI 1.0.83.", "copilot version 1.2.3", "v1.0.83", "1.0.83+build.4"} {
		if !copilotSupportsUsageOutput(reported) {
			t.Errorf("documented compatible version refused: %q", reported)
		}
	}
	for _, reported := range []string{"", "unknown", "launcher version 7.0.0", "GitHub Copilot CLI 1.0.80.", "GitHub Copilot CLI 1.0.81-0.", "0.999.999", "1.0", "1.0.83-malformed..tag"} {
		if copilotSupportsUsageOutput(reported) {
			t.Errorf("unsupported or unknown version accepted: %q", reported)
		}
	}
}

func TestCopilotUsageCompatibilityPreservesObservedSessionAccounting(t *testing.T) {
	for _, tc := range []struct {
		name, version        string
		usageFlag, malformed bool
	}{
		{name: "older CLI", version: "GitHub Copilot CLI 1.0.80."},
		{name: "unknown CLI"},
		{name: "new CLI missing usage file", version: "GitHub Copilot CLI 1.0.83.", usageFlag: true},
		{name: "new CLI malformed usage file", version: "GitHub Copilot CLI 1.0.83.", usageFlag: true, malformed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			native := []byte(`{"type":"session.shutdown","data":{"totalNanoAiu":99,"modelMetrics":{"observed":{"usage":{"inputTokens":9,"outputTokens":2,"cacheReadTokens":0},"totalNanoAiu":99}}}}` + "\n")
			// A prior attempt's valid document must never override this invocation's
			// native usage, including when the old CLI cannot create a usage file.
			if err := os.MkdirAll(filepath.Join(workspace, ".goobers"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, ".goobers", "copilot-usage.json"), readTestData(t, "copilot-usage.json"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}, act: func(req ProcessRequest) error {
				calls++
				usagePath := commandOptionValue(req.Command, "--usage-output-file")
				if (usagePath != "") != tc.usageFlag {
					t.Fatalf("incorrect usage flag negotiation: %v", req.Command)
				}
				if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
					return err
				}
				if err := writeNativeSessionLog(req, native); err != nil {
					return err
				}
				if tc.malformed {
					return os.WriteFile(filepath.Join(req.Dir, usagePath), []byte(`{"unexpected":"schema"}`), 0600)
				}
				return nil
			}}
			out, err := (&CopilotAdapter{Command: []string{"copilot"}, Runner: runner}).Run(context.Background(), RunRequest{HarnessVersion: tc.version, Envelope: testEnvelope(workspace), Workspace: workspace, CompletionPath: DefaultResultPath, Credentials: pushCredentials(t, "unused", "unused")})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("usage negotiation probed/replayed agent work: %d calls", calls)
			}
			for name, want := range map[string]float64{telemetry.AttrUsageNanoAIU: 99, telemetry.AttrGenAIUsageInputTokens: 9, telemetry.AttrGenAIUsageOutputTokens: 2, telemetry.AttrUsageCacheReadTokens: 0} {
				got, present := out.Metrics[name]
				if !present || got != want {
					t.Errorf("observed %s=%v (present=%v), want %v", name, got, present, want)
				}
			}
			if len(out.ModelUsage) != 1 || out.ModelUsage[0].Model != "observed" || out.ModelUsage[0].CostBasis != telemetry.CostBasisUnknown {
				t.Fatalf("native evidence lost or falsely upgraded: %+v", out.ModelUsage)
			}
			raw, err := os.ReadFile(filepath.Join(workspace, CopilotInvocationDiagnosticsFile))
			if err != nil {
				t.Fatal(err)
			}
			var diagnostic copilotInvocationDiagnostics
			if err := json.Unmarshal(raw, &diagnostic); err != nil {
				t.Fatal(err)
			}
			wantCapture := "session-transcript"
			if tc.usageFlag {
				wantCapture = "usage-file-with-session-fallback"
			}
			if diagnostic.UsageCapture != wantCapture {
				t.Fatalf("usage capture not diagnosed: %+v", diagnostic)
			}
		})
	}
}

func TestCopilotUsageFreshCaptureIgnoresUnremovablePriorDocumentOnProcessFailure(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if err := os.Mkdir(filepath.Join(workspace, ".goobers"), 0700); err != nil {
		t.Fatal(err)
	}
	request := RunRequest{HarnessVersion: "1.0.83", Envelope: testEnvelope(workspace), Workspace: workspace, CompletionPath: DefaultResultPath, Credentials: pushCredentials(t, "unused", "unused"), Sandbox: &stubSandbox{}}
	_, stalePath, cleanup, err := prepareCopilotUsageOutput(request, []string{"copilot"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(filepath.Dir(stalePath), 0700); err != nil {
			t.Error(err)
		}
		cleanup()
	})
	stale := readTestData(t, "copilot-usage.json")
	if err := os.WriteFile(stalePath, stale, 0600); err != nil {
		t.Fatal(err)
	}
	// Exercise an actual removal failure where POSIX permissions apply. Other
	// platforms retain the same stale document to model an interrupted cleanup.
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		if err := os.Chmod(filepath.Dir(stalePath), 0500); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(stalePath); err == nil {
			t.Fatal("fixture must prevent removal of the prior usage document")
		}
	}
	processErr := errors.New("simulated CLI failure")
	calls := 0
	currentPath := ""
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 1}, err: processErr, act: func(req ProcessRequest) error {
		calls++
		relative := commandOptionValue(req.Command, "--usage-output-file")
		if !filepath.IsLocal(relative) {
			t.Fatalf("capture is not workspace-relative: %q", relative)
		}
		currentPath = filepath.Join(workspace, relative)
		if currentPath == stalePath || filepath.Dir(filepath.Dir(currentPath)) != filepath.Join(workspace, ".goobers") {
			t.Fatalf("capture reused stale path or escaped workspace: %q", currentPath)
		}
		if _, err := os.Stat(currentPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("new capture already exists: %v", err)
		}
		return writeNativeSessionLog(req, []byte(`{"type":"session.shutdown","data":{"totalNanoAiu":99,"modelMetrics":{"observed":{"usage":{"inputTokens":9,"outputTokens":2},"totalNanoAiu":99}}}}`+"\n"))
	}}
	out, err := (&CopilotAdapter{Command: []string{"copilot"}, Runner: runner}).Run(context.Background(), request)
	if !errors.Is(err, processErr) {
		t.Fatalf("lost process failure: %v", err)
	}
	if calls != 1 {
		t.Fatalf("stage replayed: %d calls", calls)
	}
	if out.Metrics[telemetry.AttrUsageNanoAIU] != 99 || len(out.ModelUsage) != 1 || out.ModelUsage[0].CostBasis != telemetry.CostBasisUnknown {
		t.Fatalf("stale vendor document replaced native usage: %+v", out)
	}
	if _, err := os.Stat(filepath.Dir(currentPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current capture directory leaked: %v", err)
	}
	remaining, err := os.ReadFile(stalePath)
	if err != nil || string(remaining) != string(stale) {
		t.Fatalf("prior document was changed: %v", err)
	}
}

func TestCopilotUsagePreparationFailsBeforeLauncherAndCleansFailedHandshake(t *testing.T) {
	for _, blocked := range []bool{true, false} {
		t.Run(map[bool]string{true: "capture directory unavailable", false: "launcher handshake fails"}[blocked], func(t *testing.T) {
			workspace := t.TempDir()
			parent := filepath.Join(workspace, ".goobers")
			if blocked {
				if err := os.WriteFile(parent, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			calls := 0
			runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 1}, act: func(req ProcessRequest) error {
				calls++
				if commandOptionValue(req.Command, "--usage-output-file") != "" {
					t.Fatal("stage started after failed preparation")
				}
				return nil
			}}
			adapter := &CopilotAdapter{Command: []string{"wrapper"}, Runner: runner, RequireLauncherContract: true}
			_, err := adapter.prepareCopilotCaptures(context.Background(), RunRequest{HarnessVersion: "1.0.83", Workspace: workspace}, []string{"wrapper"}, nil)
			if blocked {
				if err == nil || !strings.Contains(err.Error(), "prepare fresh Copilot usage capture") || calls != 0 {
					t.Fatalf("preparation did not refuse before launcher: calls=%d err=%v", calls, err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "launcher is incompatible") || calls != 1 {
					t.Fatalf("handshake failure lost: calls=%d err=%v", calls, err)
				}
				entries, err := os.ReadDir(parent)
				if err != nil || len(entries) != 0 {
					t.Fatalf("failed handshake leaked usage capture: %v %v", entries, err)
				}
			}
		})
	}
}

func TestCopilotOlderUsageCaptureDoesNotRequireDirectory(t *testing.T) {
	for _, reported := range []string{"", "1.0.80"} {
		argv, path, cleanup, err := prepareCopilotUsageOutput(RunRequest{HarnessVersion: reported, Workspace: filepath.Join(t.TempDir(), "absent")}, []string{"copilot"})
		if err != nil || path != "" || len(argv) != 1 || argv[0] != "copilot" {
			t.Fatalf("older/unknown capture changed: %v %q %v", argv, path, err)
		}
		cleanup()
	}
}
