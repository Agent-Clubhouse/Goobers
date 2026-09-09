package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

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
