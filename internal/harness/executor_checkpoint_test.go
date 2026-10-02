package harness

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestExecutorStreamingAndTerminalOnlyTranscriptsAreIdentical(t *testing.T) {
	// The optional streaming path must not alter canonical prompt/output
	// formatting, redaction, or the terminal-only adapter fallback.
	workspace := t.TempDir()
	run := newSandboxTestRun(t)
	legacy := &fakeRecorder{dir: run.Dir()}
	secret := "registered-secret"
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte(secret))
	var finalBytes [][]byte
	for _, streaming := range []bool{false, true} {
		adapter := &FakeAdapter{Transcript: []byte("output " + secret + "\n")}
		adapter.Act = func(_ context.Context, req RunRequest) error {
			if streaming {
				chunks := []string{"output registered-sec", "ret\n"}
				offset := 0
				for _, chunk := range chunks {
					if err := req.TranscriptCheckpoint(InvocationTranscriptDelta{Source: "process-output", Invocation: 1,
						TranscriptDelta: TranscriptDelta{Offset: offset, Data: []byte(chunk), Reason: "checkpoint"}}); err != nil {
						return err
					}
					offset += len(chunk)
					assertExecutorCheckpointRedacted(t, run, "registered-sec")
				}
			}
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		}
		var recorder SpanRecorder = legacy
		if streaming {
			recorder = run
		}
		executor, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), recorder, legacy, legacy, scrubber, "instructions")
		if err != nil {
			t.Fatal(err)
		}
		result, err := executor.Invoke(t.Context(), testEnvelope(workspace))
		if err != nil {
			t.Fatal(err)
		}
		if streaming {
			reader, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			data, err := reader.SpanBytes(journal.Ref{Path: result.Transcript.Path, Digest: result.Transcript.Digest, Size: result.Transcript.Size})
			if err != nil {
				t.Fatal(err)
			}
			finalBytes = append(finalBytes, data)
		} else {
			for _, span := range legacy.spans {
				if strings.HasSuffix(span.name, ".transcript") {
					finalBytes = append(finalBytes, span.data)
				}
			}
		}
	}
	if len(finalBytes) != 2 || !bytes.Equal(finalBytes[0], finalBytes[1]) || bytes.Contains(finalBytes[1], []byte(secret)) {
		t.Fatalf("streaming changed final canonical content: %q", finalBytes)
	}
}

func assertExecutorCheckpointRedacted(t *testing.T, run *journal.Run, secretPrefix string) {
	t.Helper()
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Runner["partial"] != true {
			continue
		}
		data, err := reader.SpanBytes(*event.Ref)
		if err != nil || bytes.Contains(data, []byte(secretPrefix)) {
			t.Fatalf("unsafe intermediate transcript: %q %v", data, err)
		}
	}
}

func TestExecutorCheckpointsBeforeAdapterReturnsAndRetiresOnSuccess(t *testing.T) {
	run := newSandboxTestRun(t)
	secret := "ghp_" + strings.Repeat("A", 80)
	adapter := &FakeAdapter{Transcript: []byte("finished\n")}
	adapter.Act = func(_ context.Context, req RunRequest) error {
		if req.TranscriptCheckpoint == nil {
			t.Fatal("executor did not wire transcript checkpoint callback")
		}
		if err := req.TranscriptCheckpoint(InvocationTranscriptDelta{Source: "process-output", Invocation: 1,
			TranscriptDelta: TranscriptDelta{Data: []byte(secret + "\n"), Reason: "checkpoint"}}); err != nil {
			return err
		}
		reader, err := journal.OpenReadOnly(run.Dir())
		if err != nil {
			return err
		}
		events, err := reader.Events()
		if err != nil {
			return err
		}
		found := false
		for _, event := range events {
			if event.Type != journal.EventSpanRecorded || event.Runner["partial"] != true {
				continue
			}
			data, err := reader.SpanBytes(*event.Ref)
			if err != nil {
				return err
			}
			if string(data) != journal.Redacted+"\n" {
				t.Fatal("executor scrubber was not applied before checkpoint persistence")
			}
			found = true
		}
		if !found {
			t.Fatal("partial was not retrievable while adapter was running")
		}
		return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
	}
	executor, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), run, run,
		&fakeRecorder{dir: run.Dir()}, journal.NewPatternScrubber(), "instructions")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Invoke(t.Context(), testEnvelope(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(run.Dir(), "spans", "checkpoints"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("successful invocation retained partial artifacts: entries=%d err=%v", len(entries), err)
	}
}

func TestExecutorCanceledCaptureRetainsPartialAndReason(t *testing.T) {
	run := newSandboxTestRun(t)
	adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
		if req.TranscriptCheckpoint == nil {
			t.Fatal("missing checkpoint callback")
		}
		if err := req.TranscriptCheckpoint(InvocationTranscriptDelta{Source: "process-output", Invocation: 1,
			TranscriptDelta: TranscriptDelta{Data: []byte("work before cancellation\n"), Reason: "canceled"}}); err != nil {
			return err
		}
		return ErrCanceled
	}}
	executor, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), run, run,
		&fakeRecorder{dir: run.Dir()}, journal.NewPatternScrubber(), "instructions")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Invoke(t.Context(), testEnvelope(t.TempDir())); !errors.Is(err, ErrCanceled) {
		t.Fatalf("canceled invocation: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != journal.EventSpanRecorded || event.Runner["partial"] != true {
			continue
		}
		if event.Runner["reason"] != "canceled" {
			t.Fatal("partial lost cancellation reason")
		}
		data, err := reader.SpanBytes(*event.Ref)
		if err != nil || string(data) != "work before cancellation\n" {
			t.Fatalf("partial unavailable after reopen: %v", err)
		}
		return
	}
	t.Fatal("canceled invocation lost its partial transcript")
}
