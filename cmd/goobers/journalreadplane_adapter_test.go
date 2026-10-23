package main

import (
	"os"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
)

const crossRunTestGaggle = "acme-web"

func crossRunTestLayout(t *testing.T) instance.Layout {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return layout
}

func seedCrossRunRun(t *testing.T, layout instance.Layout, gaggle, runID string, phase journal.RunPhase) {
	t.Helper()
	runsDir := layout.ForGaggle(gaggle).RunsDir()
	run, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID, Workflow: "implementation", Gaggle: gaggle}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunJournalAdapterConcurrentReadsDoNotMutateAttachments(t *testing.T) {
	service := newDaemonRunJournalService(instance.NewLayout(t.TempDir()), nil)
	var ready, done sync.WaitGroup
	ready.Add(1)
	for range 8 {
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Wait()
			_, _ = service.RunPhase(t.Context(), journalclient.RunPhaseRequest{})
		}()
	}
	ready.Done()
	done.Wait()
}
