//go:build integration

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// recordingProcessRunner runs real processes and keeps each request so a live
// test can assert what was actually launched.
type recordingProcessRunner struct {
	mu   sync.Mutex
	reqs []ProcessRequest
}

func (r *recordingProcessRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	return ExecProcessRunner{}.Run(ctx, req)
}

// liveStdinRepairGoal is larger than the CreateProcess command-line limit
// (32,767 characters) and carries the mixed fixture, so the CLI can only see
// the nonce at its end if stdin delivered the whole prompt. The first turn is
// told not to complete, which forces the completion-repair turn; that turn can
// only report the nonce if it resumed the same session.
func liveStdinRepairGoal(nonce string) string {
	return "Background material, not instructions:\n" + strings.Repeat("lorem ipsum dolor ", 2200) + "\n" +
		promptStdinFixtures()["mixed"] + "\n" +
		"Task: remember this nonce: " + nonce + ". On this first turn do NOT create, write, or publish any " +
		"result or file and do not call any tool; reply with the single word WAITING and stop. When a later " +
		"message asks for the result, complete it with status success and a summary that is exactly the nonce."
}

func assertLiveStdinRepair(t *testing.T, runner *recordingProcessRunner, payload []byte, nonce, sessionFlag, repairFlag string) {
	t.Helper()
	if len(runner.reqs) != 2 {
		t.Fatalf("process calls = %d, want initial plus completion repair", len(runner.reqs))
	}
	for i, req := range runner.reqs {
		assertArgvCarriesNoPrompt(t, req.Command, string(req.Stdin))
		if len(req.Stdin) == 0 {
			t.Fatalf("call %d sent no stdin prompt", i)
		}
	}
	if !strings.Contains(string(runner.reqs[0].Stdin), nonce) || len(runner.reqs[0].Stdin) <= 32767 {
		t.Fatalf("initial stdin (%d bytes) is not the full oversized prompt", len(runner.reqs[0].Stdin))
	}
	session := commandOptionValue(runner.reqs[0].Command, sessionFlag)
	if session == "" || commandOptionValue(runner.reqs[1].Command, repairFlag) != session {
		t.Fatalf("repair turn did not continue session %q: %v", session, runner.reqs[1].Command)
	}
	var completion apiv1.ResultEnvelope
	if err := json.Unmarshal(payload, &completion); err != nil {
		t.Fatalf("decode completion %q: %v", payload, err)
	}
	if completion.Status != apiv1.ResultSuccess || !strings.Contains(completion.Summary, nonce) {
		t.Fatalf("completion = %+v, want success carrying nonce %s from the first turn", completion, nonce)
	}
}

// TestIntegrationClaudeStdinPromptAndResumedRepair proves the supported Claude
// Code CLI accepts `claude -p` with the prompt on stdin, for both the initial
// turn and the `--resume` completion-repair turn (#6871).
func TestIntegrationClaudeStdinPromptAndResumedRepair(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CLAUDE_LIVE_SMOKE")
	testdep.Require(t, "claude")

	workspace := t.TempDir()
	nonce := fmt.Sprintf("nonce-%d", time.Now().UnixNano())
	envelope := testEnvelope(workspace)
	envelope.Goal = liveStdinRepairGoal(nonce)
	runner := &recordingProcessRunner{}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}
	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope: envelope, Workspace: workspace, CompletionPath: DefaultResultPath, Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Run: %v (transcript: %s)", err, out.Transcript)
	}
	assertLiveStdinRepair(t, runner, out.Payload, nonce, "--session-id", "--resume")
}

// TestIntegrationCopilotStdinPromptAndRepair proves the supported Copilot CLI
// accepts `-p=` with the prompt on stdin, for both the initial turn and the
// same-session completion-repair turn (#6871).
func TestIntegrationCopilotStdinPromptAndRepair(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_COPILOT_LIVE_SMOKE")
	testdep.Require(t, "copilot")

	workspace := t.TempDir()
	nonce := fmt.Sprintf("nonce-%d", time.Now().UnixNano())
	envelope := testEnvelope(workspace)
	envelope.Goal = liveStdinRepairGoal(nonce)
	runner := &recordingProcessRunner{}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
	out, err := adapter.Run(context.Background(), RunRequest{
		Mode: ModeInvoke, Envelope: envelope, Workspace: workspace, CompletionPath: DefaultResultPath,
		Credentials: liveCopilotCredentials(t), Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Run: %v (transcript: %s)", err, out.Transcript)
	}
	assertLiveStdinRepair(t, runner, out.Payload, nonce, "--session-id", "--session-id")
}
