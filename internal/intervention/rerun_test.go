package intervention

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

func TestRerunAcceptanceRechecksStaleResolution(t *testing.T) {
	machine := interventionTestMachine(t, apiv1.EvaluatorAgentic)
	service, dir := newInterventionServiceTestRun(t, machine, "run-stale-rerun", []journal.Event{{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}})
	stale, err := service.resolve("run-stale-rerun")
	if err != nil {
		t.Fatal(err)
	}
	input := httpapi.InterventionRequest{RunID: stale.runID, Stage: "review", Actor: "operator", InstructionAddendum: "retry", IdempotencyKey: "once"}
	if err := recordInterventionMarker(stale, "rerun", input); err != nil {
		t.Fatal(err)
	}
	recorded, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatal(err)
	}
	markerEvents, err := recorded.Events()
	if err != nil {
		t.Fatal(err)
	}
	marker := markerEvents[len(markerEvents)-1]
	if marker.Runner["expectedTerminalSeq"] != float64(stale.terminalSeq) ||
		marker.Runner["expectedTerminalGeneration"] != float64(1) {
		t.Fatalf("intervention marker terminal fence = %+v", marker.Runner)
	}
	run, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{{Type: journal.EventStageRerunRequested, Stage: "review", Actor: "operator", InstructionAddendum: "retry"}, {Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	replayed, err := recheckRerunAcceptance(stale, "rerun", input)
	if err != nil || !replayed {
		t.Fatalf("stale duplicate: replayed=%v err=%v", replayed, err)
	}
	input.IdempotencyKey = "different"
	replayed, err = recheckRerunAcceptance(stale, "rerun", input)
	if err == nil || replayed {
		t.Fatalf("stale new request accepted: replayed=%v err=%v", replayed, err)
	}
}
