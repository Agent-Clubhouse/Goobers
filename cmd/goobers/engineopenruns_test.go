package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// writeRunDirectory creates the minimum on-disk shape journal.OpenRead accepts
// as a run directory.
func writeRunDirectory(t *testing.T, l instance.Layout, gaggle, runID string) {
	t.Helper()
	dir := filepath.Join(l.ForGaggle(gaggle).RunsDir(), runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.yaml"), []byte("id: "+runID+"\ndriver: engine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openInstanceLogFor(t *testing.T, l instance.Layout) *journal.InstanceLog {
	t.Helper()
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

// TestReportOrphanedEngineRunsIsSilentForAccountedRuns is the load-bearing
// half: engine.ReserveRun makes the ordering reservation-then-start, so under
// normal operation every open workflow already has a run directory. If this
// reported them anyway the signal would be pure noise and an operator would
// learn to ignore the one case that matters.
func TestReportOrphanedEngineRunsIsSilentForAccountedRuns(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	log := openInstanceLogFor(t, l)

	const directID = "goobers-web-implementation-direct"
	claimRunID := engine.RunID("goobers-web-implementation-2026-01-02T00:00:00Z")
	writeRunDirectory(t, l, "web", directID)
	writeRunDirectory(t, l, "web", claimRunID)

	orphans := reportOrphanedEngineRuns(l, log, map[string]engine.OpenRun{
		directID: {RunID: directID, WorkflowID: directID, Gaggle: "web", Workflow: "implementation"},
		claimRunID: {
			RunID: claimRunID, WorkflowID: "goobers-web-implementation-2026-01-02T00:00:00Z-run",
			Gaggle: "web", Workflow: "implementation",
		},
	})
	if len(orphans) != 0 {
		t.Fatalf("orphans = %v, want none; every open workflow has a local run directory", orphans)
	}
	if events := instanceLogEvents(t, l); len(events) != 0 {
		t.Fatalf("instance log got %d events, want none", len(events))
	}
}

// TestReportOrphanedEngineRunsMakesUnaccountedWorkflowsVisible: a workflow
// that is open for one of our gaggles with no local run directory is executing
// with nothing in this process watching it. The daemon's only correct move is
// to make it VISIBLE — it deliberately does not cancel, because a workflow
// this process cannot account for is not one to terminate on a guess.
func TestReportOrphanedEngineRunsMakesUnaccountedWorkflowsVisible(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	log := openInstanceLogFor(t, l)

	const orphanID = "goobers-web-implementation-orphan"
	writeRunDirectory(t, l, "web", "goobers-web-implementation-healthy")

	orphans := reportOrphanedEngineRuns(l, log, map[string]engine.OpenRun{
		"goobers-web-implementation-healthy": {
			RunID:      "goobers-web-implementation-healthy",
			WorkflowID: "goobers-web-implementation-healthy",
			Gaggle:     "web", Workflow: "implementation",
		},
		orphanID: {RunID: orphanID, WorkflowID: orphanID, Gaggle: "web", Workflow: "implementation"},
	})
	if len(orphans) != 1 || orphans[0] != orphanID {
		t.Fatalf("orphans = %v, want exactly [%s]", orphans, orphanID)
	}

	events := instanceLogEvents(t, l)
	if len(events) != 1 {
		t.Fatalf("instance log got %d events, want exactly one", len(events))
	}
	event := events[0]
	if event.Type != journal.EventError {
		t.Errorf("event type = %q, want %q", event.Type, journal.EventError)
	}
	if event.RunID != orphanID || event.Gaggle != "web" || event.Workflow != "implementation" {
		t.Errorf("event identity = %q/%q/%q, want web/implementation/%s", event.Gaggle, event.Workflow, event.RunID, orphanID)
	}
	if event.Error == nil || event.Error.Code != "engine_run_orphaned" {
		t.Errorf("event error = %+v, want code engine_run_orphaned", event.Error)
	}
	// The workflow id is the only handle an operator has to go look at the
	// thing, so it must survive into the record.
	if got, _ := event.Runner["workflowId"].(string); got != orphanID {
		t.Errorf("runner.workflowId = %q, want %q", got, orphanID)
	}
	if got, _ := event.Runner["driver"].(string); got != string(journal.DriverEngine) {
		t.Errorf("runner.driver = %q, want %q", got, journal.DriverEngine)
	}
}

// TestReportOrphanedEngineRunsReportsEachWorkflowOnce: a scheduled run can be
// reachable under more than one key while its claim and child are both open,
// and one workflow deserves one report.
func TestReportOrphanedEngineRunsReportsEachWorkflowOnce(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	log := openInstanceLogFor(t, l)

	const workflowID = "goobers-web-implementation-2026-01-02T00:00:00Z-run"
	orphans := reportOrphanedEngineRuns(l, log, map[string]engine.OpenRun{
		"run-a": {RunID: "run-a", WorkflowID: workflowID, Gaggle: "web"},
		"run-b": {RunID: "run-b", WorkflowID: workflowID, Gaggle: "web"},
	})
	if len(orphans) != 1 {
		t.Fatalf("orphans = %v, want one report for the single workflow", orphans)
	}
	if events := instanceLogEvents(t, l); len(events) != 1 {
		t.Fatalf("instance log got %d events, want one", len(events))
	}
}

// TestReportOrphanedEngineRunsToleratesAMissingLog: the scan is best-effort
// boot diagnostics. It must never be the reason a daemon fails to start.
func TestReportOrphanedEngineRunsToleratesAMissingLog(t *testing.T) {
	l := instance.NewLayout(t.TempDir())
	if orphans := reportOrphanedEngineRuns(l, nil, map[string]engine.OpenRun{
		"run-a": {RunID: "run-a", WorkflowID: "wf-a", Gaggle: "web"},
	}); orphans != nil {
		t.Fatalf("orphans = %v, want none without a log to report to", orphans)
	}
	if orphans := reportOrphanedEngineRuns(l, openInstanceLogFor(t, l), nil); orphans != nil {
		t.Fatalf("orphans = %v, want none for an empty scan", orphans)
	}
}

// TestOwnedGaggleSetIsExactlyWhatThisDaemonServes feeds OpenRuns' fail-closed
// filter. Over-reporting here means reattaching to a sibling instance's runs.
func TestOwnedGaggleSetIsExactlyWhatThisDaemonServes(t *testing.T) {
	owned := ownedGaggleSet(map[localscheduler.WorkflowIdentity]int{
		{Gaggle: "web", Workflow: "implementation"}:   1,
		{Gaggle: "web", Workflow: "review"}:           2,
		{Gaggle: "infra", Workflow: "implementation"}: 3,
		{Gaggle: "", Workflow: "unqualified"}:         4,
	})
	if len(owned) != 2 {
		t.Fatalf("owned = %v, want exactly web and infra", owned)
	}
	for _, gaggle := range []string{"web", "infra"} {
		if _, ok := owned[gaggle]; !ok {
			t.Errorf("gaggle %q missing from %v", gaggle, owned)
		}
	}
	if _, ok := owned[""]; ok {
		t.Error("the empty gaggle name was admitted; it matches no memo and would only widen the filter")
	}
	if empty := ownedGaggleSet(map[localscheduler.WorkflowIdentity]int(nil)); len(empty) != 0 {
		t.Errorf("ownedGaggleSet(nil) = %v, want empty", empty)
	}
}

func instanceLogEvents(t *testing.T, l instance.Layout) []journal.Event {
	t.Helper()
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	return events
}
