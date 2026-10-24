package journal

import (
	"testing"
	"time"
)

func TestRecoverStateHealsCommittedTransitions(t *testing.T) {
	prior := State{RunID: "run", Phase: PhaseRunning, MachineState: "work", LastSeq: 1,
		Branches: []BranchCursor{{Branch: 1, MachineState: "stale"}}}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	finished := []Event{{Seq: 2, Time: at, Type: EventRunFinished, Status: "completed"}}
	healed := RecoverState(prior, finished)
	if healed.Schema != StateSchema || healed.RunID != "run" || healed.Phase != PhaseCompleted || healed.MachineState != "" || len(healed.Branches) != 0 || healed.LastSeq != 2 || !healed.UpdatedAt.Equal(at) {
		t.Fatalf("terminal healing = %+v", healed)
	}
	resumed := append(finished, Event{Seq: 3, Time: at, Type: EventRunResumed, Target: "retry"})
	healed = RecoverState(healed, resumed)
	if healed.Phase != PhaseRunning || healed.MachineState != "retry" || healed.LastSeq != 3 {
		t.Fatalf("resume healing = %+v", healed)
	}
	healed.MachineState, healed.LastSeq = "later", 4
	healed = RecoverState(healed, append(resumed, Event{Seq: 4, Type: EventStageStarted, Stage: "later"}))
	if healed.MachineState != "later" {
		t.Fatalf("advanced cursor reset = %+v", healed)
	}
}
