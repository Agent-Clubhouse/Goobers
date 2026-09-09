package readservice

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestTranscriptPartialRemainsReadableWhenFinalBytesAreUnavailable(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "corrupt"}[corrupt], func(t *testing.T) {
			service, layout, machine := fixtureService(t)
			run, _ := createFixtureRun(t, layout, machine, "checkpoint-damaged-final", "default-implement", "example",
				time.Now(), journal.Trigger{Kind: journal.TriggerManual}, false)
			t.Cleanup(func() { _ = run.Close() })
			capture, err := run.BeginTranscriptCapture("implement", "copilot-cli.transcript")
			if err != nil {
				t.Fatal(err)
			}
			if err := capture.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Data: []byte("recoverable work\n"), Reason: "checkpoint"}); err != nil {
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
			var partial journal.Event
			for _, event := range events {
				if event.Runner["partial"] == true {
					partial = event
				}
			}
			if partial.Ref == nil {
				t.Fatal("missing partial fixture")
			}
			// Model the final-event-before-cleanup crash window, then damage
			// the final blob. This must not hide the remaining good copy.
			ref, err := run.RecordSpanAnnotated("implement", "copilot-cli.transcript", "", []byte("final work\n"),
				map[string]any{"transcriptCaptureComplete": partial.Runner["transcriptCapture"]})
			if err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(run.Dir(), ref.Path)
			if corrupt {
				err = os.WriteFile(filename, []byte("damaged"), 0600)
			} else {
				err = os.Remove(filename)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := service.Transcript(t.Context(), "checkpoint-damaged-final", partial.Seq)
			if err != nil || string(got.Bytes) != "recoverable work\n" {
				t.Fatalf("unavailable final hid recoverable partial: %q, %v", got.Bytes, err)
			}
		})
	}
}

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
