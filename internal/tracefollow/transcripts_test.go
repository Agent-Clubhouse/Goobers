package tracefollow

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

func TestTraceFollowTranscriptErrorsRemainVisible(t *testing.T) {
	root := t.TempDir()
	const runID = "broken-transcript-follow"
	run := newTranscriptRun(t, root, runID)
	defer func() { _ = run.Close() }()
	if _, err := run.RecordSpan("implement", "legacy.transcript", []byte("content")); err != nil {
		t.Fatal(err)
	}
	reads, err := readservice.NewOfflineRuns(instance.NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("transcript integrity failed")
	err = FollowTranscripts(t.Context(), transcriptFailureReader{OfflineRuns: reads, err: want}, runID, "", 0, true, false, time.Millisecond, io.Discard)
	if !errors.Is(err, want) {
		t.Fatalf("integrity failure hidden: %v", err)
	}
	for _, jsonOutput := range []bool{false, true} {
		err = FollowTranscripts(t.Context(), reads, runID, "", 0, true, jsonOutput, time.Millisecond, transcriptShortWriter{})
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("short output write hidden: %v", err)
		}
	}
}

type transcriptFailureReader struct {
	readservice.OfflineRuns
	err error
}

func (r transcriptFailureReader) Transcript(context.Context, string, uint64) (readservice.TranscriptContent, error) {
	return readservice.TranscriptContent{}, r.err
}

type transcriptShortWriter struct{}

func (transcriptShortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func newTranscriptRun(t *testing.T, root, runID string) *journal.Run {
	t.Helper()
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return run
}
