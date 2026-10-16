package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestCopilotAdapterSendsPromptsOnStdinNotArgv pins #6871 for the direct
// Copilot CLI path: on every platform and launcher, the initial and
// completion-repair prompts travel on stdin in full, and argv carries only the
// empty prompt-mode flag plus bounded control metadata.
func TestCopilotAdapterSendsPromptsOnStdinNotArgv(t *testing.T) {
	for name, payload := range promptStdinFixtures() {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			var calls []ProcessRequest
			runner := &fakeProcessRunner{result: ProcessResult{Transcript: []byte("finished")}}
			runner.act = func(req ProcessRequest) error {
				calls = append(calls, req)
				if len(calls) == 2 {
					return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "ok"})
				}
				return nil
			}
			adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
			envelope := testEnvelope(workspace)
			envelope.Goal = payload
			if _, err := adapter.Run(context.Background(), RunRequest{
				Mode: ModeInvoke, Envelope: envelope, Workspace: workspace, CompletionPath: DefaultResultPath, Timeout: time.Minute,
			}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(calls) != 2 {
				t.Fatalf("process calls = %d, want initial plus repair", len(calls))
			}
			rendered, err := os.ReadFile(filepath.Join(workspace, ".goobers", "prompt.md"))
			if err != nil {
				t.Fatal(err)
			}
			initial, ok := copilotSentPrompt(calls[0])
			if !ok || initial != string(rendered) || !strings.Contains(initial, payload) {
				t.Fatalf("initial stdin (%d bytes, flag %v) is not the full rendered prompt (%d bytes)", len(initial), ok, len(rendered))
			}
			repair, ok := copilotSentPrompt(calls[1])
			if !ok || !strings.Contains(repair, "ended without writing the mandatory completion file") {
				t.Fatalf("repair stdin = %q (flag %v), want the completion-repair prompt", repair, ok)
			}
			// The repair turn reuses the same argv, so it continues the same
			// --session-id rather than starting a fresh session.
			if !slices.Equal(calls[0].Command, calls[1].Command) || commandOptionValue(calls[1].Command, "--session-id") == "" {
				t.Fatalf("repair argv %v differs from initial %v or lacks the session id", calls[1].Command, calls[0].Command)
			}
			for _, call := range calls {
				assertArgvCarriesNoPrompt(t, call.Command, string(call.Stdin))
			}
		})
	}
}
