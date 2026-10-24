package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type fakeCopilotSDKRuntime struct {
	config  copilotSDKSessionConfig
	session *fakeCopilotSDKSession
	err     error
	ctx     context.Context
}

func (r *fakeCopilotSDKRuntime) OpenSession(ctx context.Context, config copilotSDKSessionConfig) (copilotSDKSession, error) {
	r.ctx = ctx
	r.config = config
	if r.err != nil {
		return nil, r.err
	}
	return r.session, nil
}

type fakeCopilotSDKSession struct {
	prompt string
	result copilotSDKResult
	err    error
	closed bool
	ctx    context.Context
}

func (s *fakeCopilotSDKSession) Complete(ctx context.Context, prompt string) (copilotSDKResult, error) {
	s.ctx = ctx
	s.prompt = prompt
	return s.result, s.err
}

func (s *fakeCopilotSDKSession) Close() error {
	s.closed = true
	return nil
}

func TestCopilotBackendBoundaryKeepsCLIAsDefault(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Summary: "cli",
			})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:              testEnvelope(workspace),
		Workspace:             workspace,
		CompletionPath:        DefaultResultPath,
		HarnessConfigResolved: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(out.Payload), `"summary":"cli"`) {
		t.Fatalf("CLI payload = %s", out.Payload)
	}
	if len(runner.lastReq.Command) == 0 || runner.lastReq.Command[0] != "copilot" {
		t.Fatalf("CLI backend command = %v", runner.lastReq.Command)
	}
}

func TestCopilotBackendBoundaryRunsSDKThroughSessionAPI(t *testing.T) {
	workspace := t.TempDir()
	completion, err := json.Marshal(apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Summary: "sdk",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeCopilotSDKSession{result: copilotSDKResult{
		Completion: completion,
		Transcript: []byte(`{"type":"assistant.message"}`),
	}}
	runtime := &fakeCopilotSDKRuntime{session: session}
	adapter := &CopilotAdapter{backend: copilotSDKBackend{runtime: runtime}}

	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:              testEnvelope(workspace, "agent:model"),
		Instructions:          "Follow the SDK path.",
		Model:                 "test-model",
		Workspace:             workspace,
		CompletionPath:        DefaultResultPath,
		Credentials:           pushCredentials(t, "agent:model", "sdk-model-token"),
		HarnessConfigResolved: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(out.Payload) != string(completion) {
		t.Fatalf("SDK payload = %s, want %s", out.Payload, completion)
	}
	if runtime.config.Workspace != workspace || runtime.config.Model != "test-model" {
		t.Fatalf("SDK session config = %+v", runtime.config)
	}
	if runtime.config.ModelCredential != "sdk-model-token" {
		t.Fatalf("SDK model credential = %q", runtime.config.ModelCredential)
	}
	if !strings.Contains(session.prompt, "Follow the SDK path.") ||
		!strings.Contains(session.prompt, "entire final response") {
		t.Fatalf("SDK prompt did not use shared response contract: %s", session.prompt)
	}
	if !session.closed {
		t.Fatal("SDK session was not closed")
	}
}

func TestCopilotSDKBackendPropagatesSessionDeadline(t *testing.T) {
	timeout := time.Minute
	session := &fakeCopilotSDKSession{result: copilotSDKResult{
		Completion: []byte(`{"status":"success","summary":"sdk"}`),
	}}
	runtime := &fakeCopilotSDKRuntime{session: session}
	adapter := &CopilotAdapter{backend: copilotSDKBackend{runtime: runtime}}

	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:              testEnvelope(t.TempDir()),
		Workspace:             t.TempDir(),
		Timeout:               timeout,
		HarnessConfigResolved: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	openDeadline, openOK := runtime.ctx.Deadline()
	completeDeadline, completeOK := session.ctx.Deadline()
	if !openOK || !completeOK {
		t.Fatalf("SDK deadlines: open=%v complete=%v", openOK, completeOK)
	}
	if !openDeadline.Equal(completeDeadline) {
		t.Fatalf("SDK deadlines differ: open=%v complete=%v", openDeadline, completeDeadline)
	}
	remaining := time.Until(openDeadline)
	if remaining <= 0 || remaining > timeout {
		t.Fatalf("SDK deadline remaining = %v, want within (0, %v]", remaining, timeout)
	}
}

func TestCopilotSDKBackendRejectsUnsupportedConfigurationBeforeRuntime(t *testing.T) {
	runtime := &fakeCopilotSDKRuntime{err: errors.New("runtime must not start")}
	adapter := &CopilotAdapter{backend: copilotSDKBackend{runtime: runtime}}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:              testEnvelope(t.TempDir()),
		Workspace:             t.TempDir(),
		Tools:                 []string{"shell"},
		HarnessConfigResolved: true,
	})
	if err == nil || !strings.Contains(err.Error(), "tool allowlists are not supported") {
		t.Fatalf("unsupported configuration error = %v", err)
	}
}
