package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

func TestTraceFollowTranscriptsDuringStage(t *testing.T) {
	for _, flag := range []string{"--transcripts", "--transcript=implement"} {
		t.Run(flag, func(t *testing.T) {
			root := t.TempDir()
			const runID = "live-transcripts"
			run := newTraceTestRun(t, root, runID)
			t.Cleanup(func() { _ = run.Close() })
			registry, scrubber := journal.DefaultScrubber()
			registry.Register([]byte("secret-value"))
			capture, err := run.BeginTranscriptCaptureWithScrubber("implement", "test.transcript", scrubber)
			if err != nil {
				t.Fatal(err)
			}
			// Deliberately split the credential at the durable checkpoint boundary.
			first := "working secret-val"
			if err := capture.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Data: []byte(first), Reason: "checkpoint"}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stdout := newTraceFollowBuffer()
			var stderr bytes.Buffer
			result := make(chan int, 1)
			go func() {
				result <- runTraceWithFollowContext(ctx, []string{"--json", "--follow", flag, runID, root}, stdout, &stderr)
			}()
			stdout.waitForWrite(t)
			if got := stdout.String(); strings.Contains(got, "secret-val") || !strings.Contains(got, "working ") {
				t.Fatalf("unsafe or missing in-flight output: %s", got)
			}
			select {
			case code := <-result:
				t.Fatalf("follow ended before the stage finished: %d %s", code, stderr.String())
			default:
			}
			if err := capture.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Offset: len(first), Data: []byte("ue\nnext\n"), Reason: "checkpoint"}); err != nil {
				t.Fatal(err)
			}
			waitForTranscriptText(t, stdout, "next")
			if strings.Contains(stdout.String(), "secret-val") {
				t.Fatal("split secret appeared in an intermediate state")
			}
			final := []byte("canonical final\n")
			if _, err := capture.RecordFinal("", final); err != nil {
				t.Fatal(err)
			}
			finishTranscriptTestRun(t, run)
			if code := waitForTraceFollow(t, result); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			records := decodeFollowTranscripts(t, stdout.String())
			if len(records) != 3 || records[0].Content != "working " || records[1].Content != journal.Redacted+"\nnext\n" || records[2].Content != string(final) {
				t.Fatalf("missing or duplicate records: %+v", records)
			}
			if records[0].Capture == "" || records[2].ReplacesCapture != records[0].Capture || !records[0].Partial || records[2].Partial {
				t.Fatalf("lost capture/final identity: %+v", records)
			}
			for i := 1; i < len(records); i++ {
				if records[i].Seq <= records[i-1].Seq {
					t.Fatal("nonmonotonic resume positions")
				}
			}
			var resumed, resumeErr bytes.Buffer
			code := runTraceWithFollowContext(t.Context(), []string{"--json", "--follow", flag, fmt.Sprintf("--after-seq=%d", records[1].Seq), runID, root}, &resumed, &resumeErr)
			if code != 0 || len(decodeFollowTranscripts(t, resumed.String())) != 1 || !strings.Contains(resumed.String(), "canonical final") {
				t.Fatalf("resume code=%d stdout=%s stderr=%s", code, resumed.String(), resumeErr.String())
			}
		})
	}
}

func TestTraceFollowTranscriptFinalizationRace(t *testing.T) {
	root := t.TempDir()
	const runID = "transcript-cleanup-race"
	run := newTraceTestRun(t, root, runID)
	t.Cleanup(func() { _ = run.Close() })
	capture, err := run.BeginTranscriptCapture("implement", "test.transcript")
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Data: []byte("partial\n"), Reason: "checkpoint"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	factory := func(layout instance.Layout) (readservice.OfflineRuns, error) {
		reads, err := readservice.NewOfflineRuns(layout)
		return &transcriptFinalizeOnRead{OfflineRuns: reads, finalize: func() {
			if _, err := capture.RecordFinal("", []byte("final\n")); err != nil {
				t.Fatal(err)
			}
			finishTranscriptTestRun(t, run)
		}}, err
	}
	code := runTraceWithFollowContextAndFactory(t.Context(), []string{"--json", "--follow", "--transcripts", runID, root}, &stdout, &stderr, factory)
	if code != 0 {
		t.Fatalf("cleanup race: code=%d stderr=%s", code, stderr.String())
	}
	if records := decodeFollowTranscripts(t, stdout.String()); len(records) != 1 || records[0].Content != "final\n" {
		t.Fatalf("cleanup race lost final: %+v", records)
	}
}

type transcriptFinalizeOnRead struct {
	readservice.OfflineRuns
	finalize func()
}

func (r *transcriptFinalizeOnRead) Transcript(ctx context.Context, runID string, seq uint64) (readservice.TranscriptContent, error) {
	if r.finalize != nil {
		finalize := r.finalize
		r.finalize = nil
		finalize()
	}
	return r.OfflineRuns.Transcript(ctx, runID, seq)
}

func TestTraceFollowTranscriptLegacySelectionAndCancellation(t *testing.T) {
	root := t.TempDir()
	const runID = "legacy-transcript-follow"
	run := newTraceTestRun(t, root, runID)
	t.Cleanup(func() { _ = run.Close() })
	for _, stage := range []string{"implement", "review"} {
		if _, err := run.RecordSpan(runID+":"+stage, "legacy.transcript", []byte(stage+" content\n")); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stdout := newTraceFollowBuffer()
	var stderr bytes.Buffer
	result := make(chan int, 1)
	go func() {
		result <- runTraceWithFollowContext(ctx, []string{"--follow", "--transcript=implement", runID, root}, stdout, &stderr)
	}()
	stdout.waitForWrite(t)
	cancel()
	if code := waitForTraceFollow(t, result); code != traceInterruptedExitCode {
		t.Fatalf("cancellation: code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "implement content") || strings.Contains(stdout.String(), "review content") {
		t.Fatalf("stage selection: %s", stdout.String())
	}
}

func TestTraceFollowTranscriptsWaitsWithoutInitialTranscript(t *testing.T) {
	root := t.TempDir()
	const runID = "empty-transcript-follow"
	run := newTraceTestRun(t, root, runID)
	defer func() { _ = run.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	code := runTraceWithFollowContext(ctx, []string{"--follow", "--transcripts", runID, root}, &stdout, &stderr)
	if code != traceInterruptedExitCode || strings.Contains(stderr.String(), "no recorded agent transcript") {
		t.Fatalf("empty capture treated as error: code=%d stderr=%s", code, stderr.String())
	}
}

func TestTraceFollowTranscriptErrorsRemainVisible(t *testing.T) {
	root := t.TempDir()
	const runID = "broken-transcript-follow"
	run := newTraceTestRun(t, root, runID)
	defer func() { _ = run.Close() }()
	if _, err := run.RecordSpan("implement", "legacy.transcript", []byte("content")); err != nil {
		t.Fatal(err)
	}
	reads, err := readservice.NewOfflineRuns(instance.NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("transcript integrity failed")
	err = followTraceTranscripts(t.Context(), transcriptFailureReader{OfflineRuns: reads, err: want}, runID, "", 0, true, false, io.Discard)
	if !errors.Is(err, want) {
		t.Fatalf("integrity failure hidden: %v", err)
	}
	for _, jsonOutput := range []bool{false, true} {
		err = followTraceTranscripts(t.Context(), reads, runID, "", 0, true, jsonOutput, transcriptShortWriter{})
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("short output write hidden: %v", err)
		}
	}
}

func TestTraceAfterSeqRequiresTranscriptFollow(t *testing.T) {
	for _, args := range [][]string{{"--after-seq=3", "run-id"}, {"--after-seq=3", "--follow", "run-id"}, {"--after-seq=3", "--transcripts", "run-id"}} {
		var stdout, stderr bytes.Buffer
		if code := runTraceWithFollowContext(t.Context(), args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--after-seq requires") {
			t.Fatalf("resume validation: code=%d stderr=%s", code, stderr.String())
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

func finishTranscriptTestRun(t *testing.T, run *journal.Run) {
	t.Helper()
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
}

func waitForTranscriptText(t *testing.T, stdout *traceFollowBuffer, text string) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !strings.Contains(stdout.String(), text) {
		select {
		case <-deadline.C:
			t.Fatalf("transcript text %q never arrived: %s", text, stdout.String())
		case <-ticker.C:
		}
	}
}

func decodeFollowTranscripts(t *testing.T, data string) []traceTranscriptRecord {
	t.Helper()
	var records []traceTranscriptRecord
	decoder := json.NewDecoder(strings.NewReader(data))
	for {
		var record traceTranscriptRecord
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			return records
		}
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
}
