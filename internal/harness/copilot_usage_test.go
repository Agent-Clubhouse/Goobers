package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
