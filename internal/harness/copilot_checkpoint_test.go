package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCopilotNativeCheckpointsSpanRecoveryAndFinishBeforeCleanup(t *testing.T) {
	workspace := t.TempDir()
	observed := make(chan struct{}, 1)
	var got []InvocationTranscriptDelta
	var path string
	calls := 0
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}}
	runner.act = func(req ProcessRequest) error {
		calls++
		for _, entry := range req.Env {
			if value, ok := strings.CutPrefix(entry, "GOOBERS_SESSION_TRANSCRIPT="); ok {
				path = value
			}
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = f.WriteString("native turn\n")
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
		if calls == 1 {
			select {
			case <-observed:
			case <-time.After(5 * time.Second):
				return errors.New("native checkpoint did not arrive while process was running")
			}
			return req.TranscriptCheckpoint(TranscriptDelta{Reason: "process-exit"})
		}
		return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
	}
	adapter := &CopilotAdapter{Command: []string{"copilot-checkpoint-test"}, Runner: runner,
		RequireLauncherContract: true, launcherContract: &launcherContract{Version: 1, SessionMode: "wrapper-managed"},
	}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope: testEnvelope(workspace), Workspace: workspace, CompletionPath: DefaultResultPath,
		Timeout: time.Minute, TranscriptCheckpointInterval: time.Millisecond,
		TranscriptCheckpoint: func(d InvocationTranscriptDelta) error {
			got = append(got, d)
			if d.Source == "copilot-session" && len(d.Data) > 0 {
				select {
				case observed <- struct{}{}:
				default:
				}
			}
			return nil
		},
	})
	if err != nil || calls != 2 {
		t.Fatalf("Run: calls=%d err=%v", calls, err)
	}
	var native string
	var last InvocationTranscriptDelta
	for _, d := range got {
		if d.Source != "copilot-session" {
			continue
		}
		if d.Invocation != 0 || d.Offset != len(native) {
			t.Fatalf("native stream restarted: %+v", d)
		}
		native += string(d.Data)
		last = d
	}
	if native != "native turn\nnative turn\n" || last.Reason != "process-exit" {
		t.Fatalf("incomplete native capture: %q last=%+v", native, last)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrapper log not cleaned up: %v", err)
	}
}

func TestCopilotNativeCheckpointFinishPreservesStorageError(t *testing.T) {
	workspace := t.TempDir()
	path := workspace + string(os.PathSeparator) + "native.jsonl"
	if err := os.WriteFile(path, []byte("native"), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("storage failed")
	req := RunRequest{Workspace: workspace, TranscriptCheckpointInterval: time.Hour,
		TranscriptCheckpoint: func(d InvocationTranscriptDelta) error {
			if d.Reason != "canceled" {
				t.Errorf("reason=%q", d.Reason)
			}
			return failure
		},
	}
	worker, err := startCopilotTranscriptCheckpoints(&req, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.finish(ErrCanceled); !errors.Is(err, failure) {
		t.Fatalf("lost storage failure: %v", err)
	}
	if _, err := worker.root.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("root descriptor leaked: %v", err)
	}
}

func TestCopilotNativeCheckpointPinsConfiguredHomeBeforeLogExists(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "copilot")
	path := copilotSessionLogPath(home, "session")
	var got []InvocationTranscriptDelta
	req := RunRequest{Workspace: t.TempDir(), TranscriptCheckpointInterval: time.Hour,
		TranscriptCheckpoint: func(d InvocationTranscriptDelta) error { got = append(got, d); return nil },
	}
	env := []string{"COPILOT_HOME=" + home}
	if _, err := startCopilotTranscriptCheckpoints(&req, filepath.Join(parent, "unrelated"), env); err == nil {
		t.Fatal("accepted sibling outside configured home")
	}
	worker, err := startCopilotTranscriptCheckpoints(&req, path, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		_ = worker.finish(err)
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("late-created native log"), 0o600); err != nil {
		_ = worker.finish(err)
		t.Fatal(err)
	}
	if err := worker.finish(ErrTimeout); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0].Data) != "late-created native log" || got[0].Reason != "timeout" {
		t.Fatalf("missing configured-home capture: %+v", got)
	}
}
