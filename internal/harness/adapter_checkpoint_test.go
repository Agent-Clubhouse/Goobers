package harness

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestAdapterTranscriptCheckpointsIdentifyRecoveryInvocation(t *testing.T) {
	for _, name := range []string{"claude", "copilot"} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			var got []InvocationTranscriptDelta
			calls := 0
			runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}}
			runner.act = func(req ProcessRequest) error {
				calls++
				if req.TranscriptCheckpoint == nil || req.TranscriptCheckpointInterval != 7*time.Second {
					return fmt.Errorf("invocation %d lost checkpoint configuration", calls)
				}
				// Each process starts its own offset at zero. Both streams
				// must survive even when their byte ranges are identical.
				if err := req.TranscriptCheckpoint(TranscriptDelta{Data: []byte("turn"), Reason: "checkpoint"}); err != nil {
					return err
				}
				if err := req.TranscriptCheckpoint(TranscriptDelta{Offset: 4, DroppedBytes: 3, Reason: "process-exit"}); err != nil {
					return err
				}
				if calls == 2 {
					return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
				}
				return nil
			}
			adapter := checkpointTestAdapter(name, runner)
			out, err := adapter.Run(context.Background(), RunRequest{
				Envelope: testEnvelope(workspace), Workspace: workspace, CompletionPath: DefaultResultPath,
				Timeout: time.Minute, TranscriptCheckpointInterval: 7 * time.Second,
				TranscriptCheckpoint: func(delta InvocationTranscriptDelta) error {
					got = append(got, delta)
					return nil
				},
			})
			if err != nil || calls != 2 || len(got) != 4 || len(out.Payload) == 0 {
				t.Fatalf("recovery checkpoint transport: calls=%d deltas=%+v err=%v", calls, got, err)
			}
			for i, delta := range got {
				if delta.Invocation != i/2+1 || delta.Source != "process-output" {
					t.Fatalf("delta %d lost stream identity: %+v", i, delta)
				}
				if i%2 == 0 {
					if delta.Offset != 0 || string(delta.Data) != "turn" || delta.Reason != "checkpoint" {
						t.Fatalf("initial delta changed: %+v", delta)
					}
				} else if delta.Offset != 4 || delta.DroppedBytes != 3 || delta.Reason != "process-exit" {
					t.Fatalf("terminal delta changed: %+v", delta)
				}
			}
		})
	}
}

func TestAdapterTranscriptCheckpointFailureDoesNotLaunchRecovery(t *testing.T) {
	for _, name := range []string{"claude", "copilot"} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			failure := errors.New("checkpoint storage failed")
			calls := 0
			runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}}
			runner.act = func(req ProcessRequest) error {
				calls++
				if req.TranscriptCheckpoint == nil {
					return errors.New("checkpoint callback missing")
				}
				return req.TranscriptCheckpoint(TranscriptDelta{Data: []byte("captured"), Reason: "process-exit"})
			}
			_, err := checkpointTestAdapter(name, runner).Run(context.Background(), RunRequest{
				Envelope: testEnvelope(workspace), Workspace: workspace, CompletionPath: DefaultResultPath,
				TranscriptCheckpoint: func(InvocationTranscriptDelta) error { return failure },
			})
			if !errors.Is(err, failure) || calls != 1 {
				t.Fatalf("checkpoint failure lost or retried: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestAbsentAdapterTranscriptCheckpointAllocatesNoCallback(t *testing.T) {
	if (RunRequest{}).processTranscriptCheckpoint(1) != nil {
		t.Fatal("absent sink produced a process callback")
	}
}

func checkpointTestAdapter(name string, runner ProcessRunner) Adapter {
	if name == "claude" {
		return &ClaudeAdapter{Command: []string{"claude-checkpoint-test"}, Runner: runner}
	}
	return &CopilotAdapter{Command: []string{"copilot-checkpoint-test"}, Runner: runner}
}
