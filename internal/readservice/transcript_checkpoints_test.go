package readservice

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestRunTranscriptsReadsPartialsAndHidesSupersededBlobs(t *testing.T) {
	service, layout, machine := fixtureService(t)
	run, _ := createFixtureRun(t, layout, machine, "checkpoint-run", "default-implement", "example",
		time.Now(), journal.Trigger{Kind: journal.TriggerManual}, false)
	t.Cleanup(func() { _ = run.Close() })
	capture, err := run.BeginTranscriptCapture("implement", "copilot-cli.transcript")
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Data: []byte("partial work\n"), Reason: "canceled"}); err != nil {
		t.Fatal(err)
	}
	partials, err := service.RunTranscripts(t.Context(), "checkpoint-run", "implement")
	if err != nil || len(partials) != 1 {
		t.Fatalf("partial read: count=%d err=%v", len(partials), err)
	}
	if !strings.Contains(partials[0].Name, "canceled") || string(partials[0].Bytes) != "partial work\n" {
		t.Fatal("partial lost content or reason")
	}
	if _, err := capture.RecordFinal("", []byte("final work\n")); err != nil {
		t.Fatal(err)
	}
	finals, err := service.RunTranscripts(t.Context(), "checkpoint-run", "implement")
	if err != nil || len(finals) != 1 || string(finals[0].Bytes) != "final work\n" {
		t.Fatalf("final read accessed deleted partials: count=%d err=%v", len(finals), err)
	}
}
