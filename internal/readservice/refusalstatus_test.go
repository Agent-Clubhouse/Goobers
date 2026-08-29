package readservice

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// TestSchedulerStatusProjectsCurrentWorkflowRefusals: workflow.refused events
// (#2860, checkpoint 3) surface for the configuration in force — the set
// resets at each daemon start and each accepted config reload, because the
// scheduler re-journals current refusals after both boundaries.
func TestSchedulerStatusProjectsCurrentWorkflowRefusals(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	for _, event := range []journal.Event{
		// A refusal from a previous daemon lifetime: superseded by the start.
		{Type: journal.EventWorkflowRefused, Gaggle: "example", Workflow: "stale", Reason: "old inventory"},
		{Type: journal.EventDaemonStarted},
		{Type: journal.EventWorkflowRefused, Gaggle: "example", Workflow: "win-build", Reason: `stage "build" requires os "windows"; no runner satisfies it`, Time: at},
		// An accepted reload re-journals current refusals; the pre-reload
		// record is stale.
		{Type: journal.EventConfigReloaded},
		{Type: journal.EventWorkflowRefused, Gaggle: "example", Workflow: "win-build", Reason: `stage "build" requires os "windows"; no runner satisfies it`, Time: at.Add(time.Minute)},
	} {
		if err := log.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := NewLocal(LocalSources{
		Layout:      layout,
		Definitions: testDefinitions(),
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}

	status, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.RefusedWorkflows) != 1 {
		t.Fatalf("RefusedWorkflows = %+v, want exactly the current refusal", status.RefusedWorkflows)
	}
	refusal := status.RefusedWorkflows[0]
	if refusal.Gaggle != "example" || refusal.Workflow != "win-build" {
		t.Fatalf("refusal identity = %+v", refusal)
	}
	if refusal.Reason == "" || refusal.Reason == "old inventory" {
		t.Fatalf("refusal must carry the current diagnostic: %+v", refusal)
	}
}
