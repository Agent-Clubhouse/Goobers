package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestDeprioritizeRepeatedFailuresPreservesEventualClaimability(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)
	ledger, err := localscheduler.OpenClaimLedger(
		filepath.Join(layout.SchedulerDir(), "claims.json"),
		localscheduler.WithLedgerClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < backlogFailureDeprioritizeThreshold; i++ {
		runID := "failed-run-" + string(rune('a'+i))
		run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		if ok, _, err := ledger.Claim("1", runID, "implementation", time.Hour); err != nil || !ok {
			t.Fatalf("claim failed: ok=%t err=%v", ok, err)
		}
		if err := ledger.Release("1", runID); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}

	items := []providers.WorkItem{{ID: "1"}, {ID: "2"}}
	got := deprioritizeRepeatedFailures(layout, ledger, items, now, backlogQueryEnv{})
	if got[0].ID != "2" || got[1].ID != "1" {
		t.Fatalf("order = %v, want healthy item before repeated failure", []string{got[0].ID, got[1].ID})
	}

	onlyFailed := deprioritizeRepeatedFailures(layout, ledger, []providers.WorkItem{{ID: "1"}}, now, backlogQueryEnv{})
	if len(onlyFailed) != 1 || onlyFailed[0].ID != "1" {
		t.Fatalf("deprioritized item = %v, want it retained as claimable", onlyFailed)
	}
}
