package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

type traceTranscriptOptions struct {
	show, follow, terminal, jsonOutput bool
	afterSeq                           uint64
}

func maybeTraceTranscripts(ctx context.Context, reads readservice.OfflineRuns, runID, stage string, options traceTranscriptOptions, newFollowContext func() (context.Context, func()), stdout, stderr io.Writer) (bool, int) {
	if !options.follow || !options.show {
		return maybePrintTraceTranscripts(ctx, reads, runID, stage, options.show, stdout, stderr)
	}
	followCtx, stop := newFollowContext()
	defer stop()
	if err := followTraceTranscripts(followCtx, reads, runID, stage, options.afterSeq, options.terminal, options.jsonOutput, stdout); err != nil {
		if errors.Is(err, context.Canceled) {
			return true, traceInterruptedExitCode
		}
		pf(stderr, "error: follow transcripts: %v\n", err)
		return true, 2
	}
	return true, 0
}

// A final transcript is a canonical replacement, not another raw delta. Its
// capture identity lets consumers replace previously displayed checkpoints.
// Older terminal-only records have no capture identity and remain standalone.
type traceTranscriptRecord struct {
	RunID           string `json:"runId"`
	Seq             uint64 `json:"seq"`
	Stage           string `json:"stage"`
	Name            string `json:"name"`
	Content         string `json:"content"`
	Partial         bool   `json:"partial,omitempty"`
	Capture         string `json:"capture,omitempty"`
	Stream          string `json:"stream,omitempty"`
	Reason          string `json:"reason,omitempty"`
	ReplacesCapture string `json:"replacesCapture,omitempty"`
}

func followTraceTranscripts(ctx context.Context, reads readservice.OfflineRuns, runID, stage string, afterSeq uint64, terminal, jsonOutput bool, stdout io.Writer) error {
	ticker := time.NewTicker(traceFollowPollInterval)
	defer ticker.Stop()
	for {
		ledger, err := reads.RunEvents(ctx, runID)
		if err != nil {
			return err
		}
		completed := verifiedFollowCaptures(ctx, reads, runID, stage, ledger.Events, afterSeq)
		for _, event := range ledger.Events {
			if err := ctx.Err(); err != nil {
				return err
			}
			if event.Seq <= afterSeq {
				continue
			}
			capture, _ := event.Runner["transcriptCapture"].(string)
			if traceTranscriptEvent(event, runID, stage) && (event.Runner["partial"] != true || !completed[capture]) {
				transcript, present, err := readFollowTranscript(ctx, reads, runID, event)
				if err != nil {
					return err
				}
				if present {
					if err := writeFollowTranscript(stdout, runID, event, transcript, jsonOutput); err != nil {
						return err
					}
				}
			}
			afterSeq = event.Seq
		}
		if terminal || traceEventsTerminal(ledger.Events) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func verifiedFollowCaptures(ctx context.Context, reads readservice.OfflineRuns, runID, stage string, events []readservice.RunEvent, afterSeq uint64) map[string]bool {
	completed := make(map[string]bool)
	for _, event := range events {
		capture, _ := event.Runner["transcriptCaptureComplete"].(string)
		if event.Seq <= afterSeq || capture == "" || !traceTranscriptEvent(event, runID, stage) {
			continue
		}
		if _, err := reads.Transcript(ctx, runID, event.Seq); err == nil {
			completed[capture] = true
		}
	}
	return completed
}

func traceTranscriptEvent(event readservice.RunEvent, runID, stage string) bool {
	if !event.KnownSchema || event.Type != journal.EventSpanRecorded || (stage != "" && stageArtifactName(runID, event.Stage) != stage) {
		return false
	}
	return event.Name == "transcript" || strings.HasSuffix(event.Name, ".transcript") ||
		(event.Runner["partial"] == true && (event.Name == "transcript.partial" || strings.HasSuffix(event.Name, ".transcript.partial")))
}

func readFollowTranscript(ctx context.Context, reads readservice.OfflineRuns, runID string, event readservice.RunEvent) (readservice.TranscriptContent, bool, error) {
	transcript, err := reads.Transcript(ctx, runID, event.Seq)
	if err == nil {
		return transcript, true, nil
	}
	// Finalization can retire the partial blob after RunEvents or after the
	// transcript reader's supersession check. Only a verified final for the
	// SAME capture authorizes skipping it; ordinary integrity errors surface.
	capture, _ := event.Runner["transcriptCapture"].(string)
	if event.Runner["partial"] != true || capture == "" {
		return readservice.TranscriptContent{}, false, err
	}
	ledger, refreshErr := reads.RunEvents(ctx, runID)
	if refreshErr != nil {
		return readservice.TranscriptContent{}, false, refreshErr
	}
	for _, candidate := range ledger.Events {
		if candidate.Runner["transcriptCaptureComplete"] == capture && traceTranscriptEvent(candidate, runID, "") {
			if _, finalErr := reads.Transcript(ctx, runID, candidate.Seq); finalErr == nil {
				return readservice.TranscriptContent{}, false, nil
			}
		}
	}
	return readservice.TranscriptContent{}, false, err
}

func writeFollowTranscript(stdout io.Writer, runID string, event readservice.RunEvent, transcript readservice.TranscriptContent, jsonOutput bool) error {
	record := traceTranscriptRecord{RunID: runID, Seq: event.Seq, Stage: transcript.Stage, Name: event.Name, Content: string(transcript.Bytes)}
	record.Partial, _ = event.Runner["partial"].(bool)
	record.Capture, _ = event.Runner["transcriptCapture"].(string)
	record.Stream, _ = event.Runner["transcriptStream"].(string)
	record.Reason, _ = event.Runner["reason"].(string)
	record.ReplacesCapture, _ = event.Runner["transcriptCaptureComplete"].(string)
	if jsonOutput {
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return writeTranscriptOutput(stdout, string(data)+"\n")
	}
	header := fmt.Sprintf("--- stage=%q name=%q seq=%d", record.Stage, record.Name, record.Seq)
	if record.Partial {
		header += fmt.Sprintf(" capture=%q stream=%q reason=%q", record.Capture, record.Stream, record.Reason)
	}
	if record.ReplacesCapture != "" {
		header += fmt.Sprintf(" replaces-capture=%q", record.ReplacesCapture)
	}
	content := header + " ---\n" + record.Content
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return writeTranscriptOutput(stdout, content)
}

func writeTranscriptOutput(stdout io.Writer, content string) error {
	n, err := io.WriteString(stdout, content)
	if err == nil && n != len(content) {
		return io.ErrShortWrite
	}
	return err
}
