package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestTelemetryPruneAcknowledgesTriggerBeforeDeletingItsJournal(t *testing.T) {
	for _, mode := range []string{"normal", "dry-run", "interrupted", "mismatched"} {
		t.Run(mode, func(t *testing.T) {
			layout := instance.NewLayout(t.TempDir())
			if err := os.MkdirAll(layout.SchedulerDir(), 0700); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			dispatch := newDaemonTriggerService()
			dispatch.now = func() time.Time { return now.Add(-48 * time.Hour) }
			s := acceptedService(t, filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"), dispatch)
			workflow := "default-implement"
			if mode == "mismatched" {
				workflow = "other-workflow"
			}
			accepted, err := s.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: workflow, Gaggle: "example", RequestID: "delivery", Actor: "operator"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.queue.BeginDispatch(t.Context(), accepted.AcceptanceID); err != nil {
				t.Fatal(err)
			}
			runID := strings.TrimPrefix(accepted.AcceptanceID, "trigger-")
			runLayout := layout.ForGaggle("example")
			runDir := createTelemetryRetentionRun(t, runLayout, runID, now.Add(-48*time.Hour))
			db, err := rollup.Open(layout.TelemetryDB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.IngestRun(t.Context(), runDir); err != nil {
				t.Fatal(err)
			}
			if mode == "interrupted" {
				staged := filepath.Join(filepath.Dir(runLayout.RunsDir()), ".telemetry-pruning", runID)
				if err := os.MkdirAll(filepath.Dir(staged), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(runDir, staged); err != nil {
					t.Fatal(err)
				}
				runDir = staged
			}
			_, err = pruneTelemetryRetention(layout, instance.TelemetryRetentionConfig{Window: "24h", MaxRuns: 500}, db, now, mode == "dry-run")
			if mode == "mismatched" {
				if err == nil {
					t.Fatal("mismatched receipt allowed deletion")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			record, err := s.queue.Get(t.Context(), accepted.AcceptanceID, "operator")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "dry-run" || mode == "mismatched" {
				if record.State != triggerqueue.Dispatching {
					t.Fatalf("unproven dispatch acknowledged: %+v", record)
				}
				if _, err := os.Stat(runDir); err != nil {
					t.Fatalf("journal lost: %v", err)
				}
				return
			}
			if record.State != triggerqueue.Dispatched || record.RunID != runID {
				t.Fatalf("journal deleted without durable receipt: %+v", record)
			}
			if _, err := os.Stat(runDir); !os.IsNotExist(err) {
				t.Fatalf("journal not pruned: %v", err)
			}
		})
	}
}
