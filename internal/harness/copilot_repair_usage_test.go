package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
)

func copilotRepairUsageFixture(t *testing.T, model string, input int64) []byte {
	t.Helper()
	metric := map[string]any{"usage": map[string]any{"inputTokens": input, "outputTokens": 2, "cacheReadTokens": 0}, "totalNanoAiu": input * 100}
	models := map[string]any{model: metric}
	data, err := json.Marshal(map[string]any{
		"totalNanoAiu": input * 100, "modelMetrics": models,
		"agentMetrics": map[string]any{"main": map[string]any{"totalNanoAiu": input * 100, "modelMetrics": models}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCopilotRepairRetainsEachSessionUsageOnce(t *testing.T) {
	for _, tc := range []struct {
		name                                        string
		file, missing, malformed, failed, sameModel bool
	}{
		{name: "usage file supersedes native snapshot", file: true},
		{name: "native fallback"},
		{name: "identical snapshots are different sessions", file: true, sameModel: true},
		{name: "missing repair usage", file: true, missing: true},
		{name: "malformed repair document falls back", file: true, malformed: true, sameModel: true},
		{name: "failed repair retains measured usage", file: true, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			workspace := t.TempDir()
			version := "GitHub Copilot CLI 1.0.80."
			if tc.file {
				version = "GitHub Copilot CLI 1.0.83."
			}
			calls := 0
			var observed map[string]float64
			var initialID, initialUsagePath string
			runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}, act: func(req ProcessRequest) error {
				calls++
				id := commandOptionValue(req.Command, "--session-id")
				usagePath := commandOptionValue(req.Command, "--usage-output-file")
				if calls == 1 {
					initialID, initialUsagePath = id, usagePath
				} else {
					if id == initialID || (tc.file && usagePath == initialUsagePath) {
						t.Fatal("repair reused a capture")
					}
					if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
						return err
					}
					if tc.missing {
						return nil
					}
				}
				model := "first-model"
				if calls == 2 && !tc.sameModel {
					model = "repair-model"
				}
				data := copilotRepairUsageFixture(t, model, 9)
				native := append(append([]byte(`{"type":"session.shutdown","data":`), data...), []byte("}\n")...)
				if err := writeNativeSessionLog(req, native); err != nil {
					return err
				}
				if tc.file {
					if tc.malformed && calls == 2 {
						data = []byte(`{"invalid":true}`)
					}
					if err := os.WriteFile(filepath.Join(req.Dir, usagePath), data, 0600); err != nil {
						return err
					}
				}
				if tc.failed && calls == 2 {
					return errors.New("repair failed")
				}
				return nil
			}}
			out, err := (&CopilotAdapter{Command: []string{"copilot"}, Runner: runner}).Run(context.Background(), RunRequest{
				HarnessVersion: version, Envelope: testEnvelope(workspace), Workspace: workspace,
				CompletionPath: DefaultResultPath, Credentials: pushCredentials(t, "unused", "unused"),
				UsageSink: func(metrics map[string]float64) {
					observed = metrics
				},
			})
			if (err != nil) != tc.failed {
				t.Fatalf("Run error = %v", err)
			}
			want := float64(18)
			if tc.missing {
				want = 9
			}
			if calls != 2 || out.Metrics[telemetry.AttrGenAIUsageInputTokens] != want || out.Metrics[telemetry.AttrUsageNanoAIU] != want*100 {
				t.Fatalf("calls=%d usage=%v, want input=%v nanoAIU=%v", calls, out.Metrics, want, want*100)
			}
			if observed[telemetry.AttrGenAIUsageInputTokens] != want || observed[telemetry.AttrUsageNanoAIU] != want*100 {
				t.Fatalf("streamed usage=%v, want input=%v nanoAIU=%v", observed, want, want*100)
			}
			var modelInput int64
			for _, model := range out.ModelUsage {
				modelInput += *model.InputTokens
			}
			if float64(modelInput) != want {
				t.Fatalf("model usage lost: %+v", out.ModelUsage)
			}
			var transcriptInput int64
			for _, line := range bytes.Split(bytes.TrimSpace(out.Transcript), []byte("\n")) {
				var event transcriptEvent
				if err := json.Unmarshal(line, &event); err != nil {
					t.Fatal(err)
				}
				if event.Usage != nil && event.Usage.InputTokens != nil {
					transcriptInput += *event.Usage.InputTokens
				}
			}
			if float64(transcriptInput) != want {
				t.Fatalf("transcript lost a session's usage: %s", out.Transcript)
			}
			wantBasis := telemetry.CostBasisVendorReported
			if tc.malformed {
				wantBasis = telemetry.CostBasisUnknown
			}
			if tc.sameModel && (len(out.ModelUsage) != 1 || out.ModelUsage[0].CostBasis != wantBasis) {
				t.Fatalf("model aggregation = %+v", out.ModelUsage)
			}
		})
	}
}

func TestCopilotRepairTranscriptKeepsPartialTailTolerance(t *testing.T) {
	var paths []string
	for _, data := range []string{
		"{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"first\",\"content\":\"initial\"}}\n{\"type\":",
		"{\"type\":\"assistant.message\",\"data\":{\"messageId\":\"second\",\"content\":\"repair\"}}\n{\"type\":",
	} {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, ok := readCopilotSessionTranscript(path, 1<<20); !ok {
			t.Fatal("individual partial-tail transcript rejected")
		}
		paths = append(paths, path)
	}
	native, ok := readCopilotSessionTranscripts(paths, 1<<20)
	if !ok || !bytes.Contains(native.data, []byte("initial")) || !bytes.Contains(native.data, []byte("repair")) {
		t.Fatalf("lost transcripts around partial tail: ok=%v data=%s", ok, native.data)
	}
}

func TestCopilotRepairRotatesWrapperExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.jsonl")
	usage := &copilotRepairUsage{transcriptPath: path}
	argv, env, err := usage.prepare(context.Background(), &fakeProcessRunner{}, RunRequest{}, []string{"wrapper"}, []string{"GOOBERS_SESSION_TRANSCRIPT=" + path})
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 1 || usage.transcriptPath == path || env[0] != "GOOBERS_SESSION_TRANSCRIPT="+usage.transcriptPath {
		t.Fatalf("argv=%v env=%v usage=%+v", argv, env, usage)
	}
}

func TestCopilotRepairCallerManagedSessionKeepsSnapshotSemantics(t *testing.T) {
	usage := &copilotRepairUsage{usagePath: filepath.Join(t.TempDir(), "usage.json")}
	command := []string{"copilot", "--resume", "caller-session"}
	argv, _, err := usage.prepare(context.Background(), &fakeProcessRunner{}, RunRequest{}, command, nil)
	if err != nil || !slices.Equal(argv, command) || usage.previous != nil || filepath.Base(usage.usagePath) != "usage.json" {
		t.Fatalf("caller-managed session must keep final-snapshot accounting: argv=%v usage=%+v err=%v", argv, usage, err)
	}
}

func assertCopilotRepairSessionArguments(t *testing.T, initial, repair []string) {
	t.Helper()
	firstID := commandOptionValue(initial, "--session-id")
	repairID := commandOptionValue(repair, "--session-id")
	if firstID == "" || repairID == "" || firstID == repairID {
		t.Fatalf("repair must have a fresh session: %v / %v", initial, repair)
	}
	normalized := append([]string(nil), repair...)
	for i, arg := range normalized {
		if arg == "--session-id" && i+1 < len(normalized) {
			normalized[i+1] = firstID
		}
	}
	if !slices.Equal(initial, normalized) {
		t.Fatalf("non-session arguments changed: %v / %v", initial, repair)
	}
}
