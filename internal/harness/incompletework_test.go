package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

const incompleteWorkResult = `{"status":"failure","outputs":{"implemented":false},"summary":"Setup succeeded; implementation remains unfinished within this response.","error":{"code":"INCOMPLETE_WORK","message":"Code changes, verification and commit are not completed."}}`

func TestExecutorContinuesIncompleteWorkOnce(t *testing.T) {
	crashed := errors.New("continuation process failed")
	for _, tc := range []struct {
		name, next string
		processErr error
		wantStatus apiv1.ResultStatus
		wantCode   string
	}{
		{name: "finishes work", next: `{"status":"success","summary":"Implemented and verified."}`, wantStatus: apiv1.ResultSuccess},
		{name: "still unfinished", next: incompleteWorkResult, wantStatus: apiv1.ResultFailure, wantCode: "INCOMPLETE_WORK"},
		{name: "concrete failure", next: `{"status":"failure","summary":"Verification failed.","error":{"code":"TEST_FAILED","message":"Assertion failed."}}`, wantStatus: apiv1.ResultFailure, wantCode: "TEST_FAILED"},
		{name: "process failure", processErr: crashed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("COPILOT_HOME", "")
			workspace := t.TempDir()
			var calls []ProcessRequest
			runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}}
			runner.act = func(req ProcessRequest) error {
				calls = append(calls, req)
				if len(calls) > 2 {
					t.Fatal("incomplete work received more than one continuation")
				}
				payload := incompleteWorkResult
				input := int64(5)
				if len(calls) == 1 {
					if err := os.WriteFile(filepath.Join(req.Dir, "setup-completed"), []byte("ready"), 0600); err != nil {
						return err
					}
				} else {
					if _, err := os.Stat(filepath.Join(req.Dir, "setup-completed")); err != nil {
						return err
					}
					if tc.processErr != nil {
						return tc.processErr
					}
					payload, input = tc.next, 9
				}
				data := copilotRepairUsageFixture(t, "test-model", input)
				if err := writeNativeSessionLog(calls[0], append(append([]byte(`{"type":"session.shutdown","data":`), data...), []byte("}\n")...)); err != nil {
					return err
				}
				_, err := req.StdoutCapture.Write([]byte(payload))
				return err
			}
			rec := &fakeRecorder{}
			executor, err := NewExecutor(
				&CopilotAdapter{Command: []string{"copilot"}, Runner: runner},
				testInjector(t, "", "", noopRegistrar{}), rec, rec, rec,
				journal.NewRegistryScrubber(), "", WithTools([]string{"view"}),
			)
			if err != nil {
				t.Fatal(err)
			}
			env := testEnvelope(workspace)
			result, err := executor.Invoke(context.Background(), env)
			if tc.processErr != nil {
				if !errors.Is(err, crashed) {
					t.Fatalf("Invoke error = %v, want continuation process failure", err)
				}
			} else if err != nil || result.Status != tc.wantStatus ||
				(tc.wantCode != "" && (result.Error == nil || result.Error.Code != tc.wantCode)) {
				t.Fatalf("Invoke = (%+v, %v)", result, err)
			}
			if len(calls) != 2 {
				t.Fatalf("process calls = %d, want initial plus one continuation", len(calls))
			}
			if first := commandOptionValue(calls[0].Command, "--session-id"); first == "" ||
				commandOptionValue(calls[1].Command, "--resume") != first {
				t.Fatalf("continuation did not resume original session: %v / %v", calls[0].Command, calls[1].Command)
			}
			if calls[1].Dir != calls[0].Dir || !reflect.DeepEqual(calls[1].Env, calls[0].Env) {
				t.Fatal("continuation changed the workspace or credential environment")
			}
			if calls[1].Timeout <= 0 || calls[1].Timeout > calls[0].Timeout {
				t.Fatalf("continuation timeout = %s, initial = %s", calls[1].Timeout, calls[0].Timeout)
			}
			if prompt := string(calls[1].Stdin); !strings.Contains(prompt, "INCOMPLETE_WORK") ||
				!strings.Contains(prompt, "Continue the remaining task using tools") ||
				!strings.Contains(prompt, "Do not repeat completed") {
				t.Fatalf("continuation prompt = %q", prompt)
			}
			if tc.processErr == nil && result.Metrics[telemetry.AttrGenAIUsageInputTokens] != 9 {
				t.Fatalf("cumulative usage = %v, want latest session total 9, not sum of snapshots", result.Metrics)
			}
			for _, artifact := range rec.artifacts {
				if strings.HasSuffix(artifact.name, "/invalid-completion.json") {
					t.Fatal("schema-valid incomplete work was recorded as invalid JSON")
				}
			}
		})
	}
}

func TestExecutorDoesNotContinueConcreteFailures(t *testing.T) {
	for _, payload := range []string{
		`{"status":"failure","error":{"code":"SETUP_FAILED","message":"Dependency restore failed."}}`,
		`{"status":"failure","error":{"code":"ACCESS_DENIED","message":"Provider denied access."}}`,
		`{"status":"failure","error":{"code":"CONTENT_EXCLUDED","message":"Required content is excluded."}}`,
		`{"status":"blocked","error":{"code":"INCOMPLETE_WORK","message":"Requires operator intervention."}}`,
		`{"status":"failure","error":{"code":"INCOMPLETE_WORK","message":"Cannot continue.","retryable":false}}`,
		`{"status":"failure","error":{"code":"INCOMPLETE_WORK","message":"Tests failed.","causes":[{"code":"TEST_FAILED","message":"Assertion failed."}]}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			t.Setenv("HOME", "")
			t.Setenv("COPILOT_HOME", "")
			calls := 0
			runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}, act: func(req ProcessRequest) error {
				calls++
				_, err := req.StdoutCapture.Write([]byte(payload))
				return err
			}}
			workspace := t.TempDir()
			out, err := (&CopilotAdapter{Command: []string{"copilot"}, Runner: runner}).Run(context.Background(), RunRequest{
				Mode: ModeInvoke, Envelope: testEnvelope(workspace), Workspace: workspace,
				CompletionPath: DefaultResultPath, Timeout: time.Minute, Tools: []string{"view"},
			})
			if err != nil || calls != 1 || string(out.Payload) != payload {
				t.Fatalf("Run calls=%d payload=%s error=%v", calls, out.Payload, err)
			}
		})
	}
}

type incompleteWorkControlledSession struct {
	repairCredentialSession
	calls int
}

func (s *incompleteWorkControlledSession) RunPrompt(_ context.Context, _ string, req ProcessRequest) (ProcessResult, error) {
	s.calls++
	payload := incompleteWorkResult
	if s.calls == 2 {
		payload = `{"status":"success","summary":"Finished in original session."}`
	}
	_, err := req.StdoutCapture.Write([]byte(payload))
	return ProcessResult{}, err
}

func TestCopilotIncompleteWorkKeepsLiveControlledSession(t *testing.T) {
	workspace := t.TempDir()
	session := &incompleteWorkControlledSession{}
	opened := 0
	runner := &copilotControlledRunner{
		request:     RunRequest{Workspace: workspace, Tools: append([]string{"view"}, goobersIOAvailableToolNames()...)},
		promptIndex: 0,
		factory: func(context.Context, ProcessRequest, *copilotControlledRunner) (copilotModelSession, error) {
			opened++
			return session, nil
		},
	}
	t.Cleanup(runner.close)
	initial := ProcessRequest{
		Command: []string{copilotPromptArg(defaultPromptFlag, "")}, Dir: workspace,
		Stdin: []byte("Implement the task."), Timeout: time.Minute,
		StdoutCapture: newTranscriptBuffer(1 << 16),
	}
	if _, err := runner.Run(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	usage := &copilotRepairUsage{}
	refreshed := false
	_, payload, runErr, completionErr := runCopilotCompletionRepair(
		context.Background(), runner,
		RunRequest{Mode: ModeInvoke, Workspace: workspace, Timeout: time.Minute},
		ProcessResult{}, []byte(incompleteWorkResult), initial.Command, nil,
		func(context.Context, []string, RunRequest) ([]string, error) {
			refreshed = true
			return nil, errors.New("live session must not rotate credentials")
		},
		true, "", time.Now(), errIncompleteWork, &adapterAgentEmitter{}, usage,
	)
	if runErr != nil || completionErr != nil || !strings.Contains(string(payload), `"status":"success"`) {
		t.Fatalf("continuation payload=%s runErr=%v completionErr=%v", payload, runErr, completionErr)
	}
	if opened != 1 || session.calls != 2 || session.probeCall != 1 || refreshed || usage.previous != nil {
		t.Fatalf("opened=%d turns=%d probes=%d refreshed=%v previousUsage=%v", opened, session.calls, session.probeCall, refreshed, usage.previous)
	}
}

func TestCopilotIncompleteWorkStopsAtCancellationAndDeadline(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, finish := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer finish()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		started time.Time
		wantErr error
	}{
		{"canceled", canceled, time.Now(), ErrCanceled},
		{"expired context", expired, time.Now(), ErrTimeout},
		{"spent stage budget", context.Background(), time.Now().Add(-time.Hour), ErrTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeProcessRunner{act: func(ProcessRequest) error {
				t.Fatal("continuation ran after cancellation or budget exhaustion")
				return nil
			}}
			_, payload, runErr, completionErr := runCopilotCompletionRepair(
				tc.ctx, runner, RunRequest{Timeout: time.Minute}, ProcessResult{},
				[]byte(incompleteWorkResult), []string{"copilot", "--session-id", "original"}, nil,
				nil, true, "", tc.started, errIncompleteWork, nil, nil,
			)
			if !errors.Is(runErr, tc.wantErr) || completionErr != nil || string(payload) != incompleteWorkResult {
				t.Fatalf("payload=%s runErr=%v completionErr=%v", payload, runErr, completionErr)
			}
		})
	}
}

func TestFileCompletionAdaptersContinueIncompleteWork(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			var calls []ProcessRequest
			runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}}
			runner.act = func(req ProcessRequest) error {
				calls = append(calls, req)
				payload := incompleteWorkResult
				if len(calls) == 2 {
					payload = `{"status":"success","summary":"Finished."}`
				} else if len(calls) > 2 {
					t.Fatal("more than one continuation")
				}
				return writeRaw(req.Dir, DefaultResultPath, payload)
			}
			var adapter Adapter
			request := RunRequest{
				Mode: ModeInvoke, Envelope: testEnvelope(workspace), Workspace: workspace,
				CompletionPath: DefaultResultPath, Timeout: time.Minute,
				ValidateCompletion: func(payload []byte) error {
					return json.Unmarshal(payload, &apiv1.ResultEnvelope{})
				},
			}
			if name == "claude" {
				stubClaudeCredentialsHome(t)
				runner.result.Transcript = []byte(claudeResultStream)
				adapter = &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}
			} else {
				runner.result.Transcript = []byte(codexCompletedStream)
				request.Credentials = pushCredentials(t, "agent:model", "sk-test-codex")
				request.Envelope.Capabilities = []string{"agent:model"}
				adapter = &CodexAdapter{
					Command: []string{"codex"}, Runner: runner,
					EnvCapabilities: map[string]string{"agent:model": codexModelEnv},
				}
			}
			out, err := adapter.Run(context.Background(), request)
			if err != nil || !claimsSuccess(out.Payload) || len(calls) != 2 {
				t.Fatalf("Run calls=%d payload=%s error=%v", len(calls), out.Payload, err)
			}
			if calls[0].Dir != calls[1].Dir || calls[1].Timeout <= 0 || calls[1].Timeout > calls[0].Timeout {
				t.Fatal("continuation did not retain the workspace and original timeout budget")
			}
			if name == "claude" {
				if commandOptionValue(calls[0].Command, "--session-id") != commandOptionValue(calls[1].Command, "--resume") {
					t.Fatal("Claude continuation lost its session")
				}
			} else if !slices.Contains(calls[1].Command, "resume") || !slices.Contains(calls[1].Command, "thread-1") {
				t.Fatal("Codex continuation lost its thread")
			}
		})
	}
}

func TestIncompleteWorkSharesExistingRepairAllowance(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("COPILOT_HOME", "")
	calls := 0
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}, act: func(req ProcessRequest) error {
		calls++
		payload := `{"status":"not-a-status"}`
		if calls == 2 {
			payload = incompleteWorkResult
		} else if calls > 2 {
			t.Fatal("incomplete work stacked a continuation on a schema repair")
		}
		_, err := req.StdoutCapture.Write([]byte(payload))
		return err
	}}
	rec := &fakeRecorder{}
	executor, err := NewExecutor(
		&CopilotAdapter{Command: []string{"copilot"}, Runner: runner},
		testInjector(t, "", "", noopRegistrar{}), rec, rec, rec,
		journal.NewRegistryScrubber(), "", WithTools([]string{"view"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Invoke(context.Background(), testEnvelope(t.TempDir()))
	if err != nil || calls != 2 || result.Status != apiv1.ResultFailure ||
		result.Error == nil || result.Error.Code != "INCOMPLETE_WORK" {
		t.Fatalf("Invoke calls=%d result=%+v error=%v", calls, result, err)
	}
}

func TestIncompleteWorkDoesNotHideOtherAdapterErrors(t *testing.T) {
	captureErr := errors.New("transcript capture failed")
	kept := withoutPostcondition(errors.Join(errIncompleteWork, captureErr), errIncompleteWork)
	if !errors.Is(kept, captureErr) || errors.Is(kept, errIncompleteWork) {
		t.Fatalf("settled error = %v, want only capture failure", kept)
	}
	if _, err := copilotIncompleteWorkCommand(&fakeProcessRunner{}, []string{"custom-launcher"}); err == nil {
		t.Fatal("a launcher without a resumable session must not start fresh work")
	}
}
