package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestAcceptedTriggerObserverFindsStagedJournalWithoutRunsRoot(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	scoped := layout.ForGaggle("example")
	const runID = "1234567890abcdef1234567890abcdef"
	dir := createTelemetryRetentionRun(t, scoped, runID, time.Now().Add(-48*time.Hour))
	staged := filepath.Join(filepath.Dir(scoped.RunsDir()), ".telemetry-pruning", runID)
	if err := os.MkdirAll(filepath.Dir(staged), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, staged); err != nil {
		t.Fatal(err)
	}
	// The parent is empty: removing it reproduces the discovery blind spot,
	// not a deletion of any run content.
	if err := os.Remove(scoped.RunsDir()); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(acceptedTriggerPayload{Request: httpapi.TriggerRequest{Workflow: "default-implement", Gaggle: "example"}})
	if err != nil {
		t.Fatal(err)
	}
	observed, err := acceptedTriggerObserver(layout)(t.Context(), triggerqueue.Record{ID: "trigger-" + runID, Payload: payload})
	if err != nil || !observed {
		t.Fatalf("staged run considered absent: %v, %v", observed, err)
	}
}

func TestAcceptedTriggerJournalDirRejectsMalformedOrAmbiguousLocations(t *testing.T) {
	for _, mode := range []string{"file-run", "file-staging-root", "duplicate", "invalid-id", "missing"} {
		t.Run(mode, func(t *testing.T) {
			layout := instance.NewLayout(t.TempDir())
			const runID = "1234567890abcdef1234567890abcdef"
			id := runID
			runDir := filepath.Join(layout.RunsDir(), runID)
			staging := filepath.Join(layout.Root, ".telemetry-pruning")
			switch mode {
			case "file-run":
				if err := os.MkdirAll(layout.RunsDir(), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(runDir, []byte("not a journal"), 0600); err != nil {
					t.Fatal(err)
				}
			case "file-staging-root":
				if err := os.WriteFile(staging, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				for _, dir := range []string{runDir, filepath.Join(staging, runID)} {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
			case "invalid-id":
				id = "../escape"
			}
			dir, err := acceptedTriggerJournalDir(t.Context(), layout, id)
			if mode == "missing" {
				if err != nil || dir != "" {
					t.Fatalf("missing = %q, %v", dir, err)
				}
			} else if err == nil || dir != "" {
				t.Fatalf("unsafe location accepted: %q, %v", dir, err)
			}
		})
	}
}
