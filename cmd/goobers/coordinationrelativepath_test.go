package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestDaemonCoordinationSupportsRelativeInstanceRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	layout := instance.NewLayout("relative-instance")
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	triggers, _, cancels, err := newDaemonCoordinationServices(layout, newDaemonTriggerService(), newDaemonRunnerRegistry(), log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = triggers.queue.Close(); _ = cancels.receipts.Close() }()
	if _, err := triggers.Trigger(t.Context(), httpapi.TriggerRequest{Workflow: "impl", RequestID: "relative", Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cancels.Cancel(t.Context(), httpapi.CancelRunRequest{RunID: "absent", IdempotencyKey: "relative", Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	_, closeGuard, err := openTriggerPruneGuard(layout, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	closeGuard()
	for _, name := range []string{"accepted-triggers.db", "cancellation-receipts.db"} {
		if _, err := os.Stat(filepath.Join(layout.SchedulerDir(), name)); err != nil {
			t.Fatal(err)
		}
	}
}
