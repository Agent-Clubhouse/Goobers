package goobernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestKillMatrixHasSixCells(t *testing.T) {
	cells := KillMatrix()
	if len(cells) != 6 {
		t.Fatalf("KillMatrix() has %d cells, want 6 (3 stage classes x 2 failure kinds)", len(cells))
	}
	seen := make(map[KillMatrixCell]bool)
	for _, c := range cells {
		if seen[c] {
			t.Fatalf("duplicate cell %s", c)
		}
		seen[c] = true
	}
	for _, stage := range AllStageClasses() {
		for _, failure := range AllFailureKinds() {
			if !seen[KillMatrixCell{Stage: stage, Failure: failure}] {
				t.Fatalf("KillMatrix() is missing cell %s/%s", stage, failure)
			}
		}
	}
}

func infraAttempt(pod, node string, errCode string) readservice.StageAttempt {
	return readservice.StageAttempt{
		Class: string(journal.AttemptInfra),
		Placement: &journal.Placement{
			Runner: "r", Pod: pod, Node: node, OS: "linux",
		},
		Error: &journal.ErrorDetail{Code: errCode},
	}
}

func TestClassifyCellResultPass(t *testing.T) {
	successor := placedAttempt(2, string(journal.AttemptInfra), "pod-2", "node-b", "linux")
	record := CellInjectionRecord{
		Cell:                     KillMatrixCell{Stage: StageClassBuiltin, Failure: FailureKindPodKill},
		InjectedAt:               time.Now(),
		InjectedTarget:           "pod-1",
		InterruptedAttempt:       infraAttempt("pod-1", "node-a", telemetry.ErrCodeInfraNet),
		SuccessorAttempt:         &successor,
		RunCompletedSuccessfully: true,
	}
	got := ClassifyCellResult(record)
	if got.Verdict != VerdictPass {
		t.Fatalf("Verdict = %v, want pass; detail=%q", got.Verdict, got.Detail)
	}
}

// TestClassifyCellResultCatchesThe3361RegressionClass is S6's explicit named
// fail condition: an interrupted attempt classified as policy/work, not
// infra.
func TestClassifyCellResultCatchesThe3361RegressionClass(t *testing.T) {
	successor := placedAttempt(2, string(journal.AttemptPolicy), "pod-2", "node-b", "linux")
	interrupted := infraAttempt("pod-1", "node-a", telemetry.ErrCodeInfraNet)
	interrupted.Class = string(journal.AttemptPolicy) // the regression: infra kill journaled as policy
	record := CellInjectionRecord{
		Cell:                     KillMatrixCell{Stage: StageClassAgentic, Failure: FailureKindNodeKill},
		InjectedAt:               time.Now(),
		InjectedTarget:           "node-a",
		InterruptedAttempt:       interrupted,
		SuccessorAttempt:         &successor,
		RunCompletedSuccessfully: true,
	}
	got := ClassifyCellResult(record)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (#3361 regression class)", got.Verdict)
	}
}

func TestClassifyCellResultFailsWhenRunNeverCompletes(t *testing.T) {
	successor := placedAttempt(2, string(journal.AttemptInfra), "pod-2", "node-b", "linux")
	record := CellInjectionRecord{
		Cell:                     KillMatrixCell{Stage: StageClassLocalCI, Failure: FailureKindPodKill},
		InjectedAt:               time.Now(),
		InjectedTarget:           "pod-1",
		InterruptedAttempt:       infraAttempt("pod-1", "node-a", telemetry.ErrCodeInfraNet),
		SuccessorAttempt:         &successor,
		RunCompletedSuccessfully: false,
	}
	got := ClassifyCellResult(record)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (run never completed)", got.Verdict)
	}
}

func TestClassifyCellResultFailsWhenSuccessorReusesPod(t *testing.T) {
	successor := placedAttempt(2, string(journal.AttemptInfra), "pod-1", "node-b", "linux") // same pod
	record := CellInjectionRecord{
		Cell:                     KillMatrixCell{Stage: StageClassBuiltin, Failure: FailureKindPodKill},
		InjectedAt:               time.Now(),
		InjectedTarget:           "pod-1",
		InterruptedAttempt:       infraAttempt("pod-1", "node-a", telemetry.ErrCodeInfraNet),
		SuccessorAttempt:         &successor,
		RunCompletedSuccessfully: true,
	}
	got := ClassifyCellResult(record)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (successor reused pod)", got.Verdict)
	}
}

func TestClassifyCellResultInvalidWhenNoInjectionRecorded(t *testing.T) {
	got := ClassifyCellResult(CellInjectionRecord{Cell: KillMatrixCell{Stage: StageClassBuiltin, Failure: FailureKindPodKill}})
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid (D5: every injection must be recorded)", got.Verdict)
	}
}

func TestRunKillMatrixWithStaticCellDriver(t *testing.T) {
	successor := placedAttempt(2, string(journal.AttemptInfra), "pod-2", "node-b", "linux")
	records := make(map[KillMatrixCell]CellInjectionRecord, 6)
	for _, cell := range KillMatrix() {
		records[cell] = CellInjectionRecord{
			InjectedAt:               time.Now(),
			InjectedTarget:           "pod-1",
			InterruptedAttempt:       infraAttempt("pod-1", "node-a", telemetry.ErrCodeInfraNet),
			SuccessorAttempt:         &successor,
			RunCompletedSuccessfully: true,
		}
	}
	driver := NewStaticCellDriver(records)
	got, err := RunKillMatrix(context.Background(), driver, "run-1")
	if err != nil {
		t.Fatalf("RunKillMatrix: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("RunKillMatrix returned %d records, want 6", len(got))
	}
	for _, record := range got {
		if record.RunID != "run-1" {
			t.Fatalf("record.RunID = %q, want run-1", record.RunID)
		}
		if verdict := ClassifyCellResult(record); verdict.Verdict != VerdictPass {
			t.Fatalf("cell %s classified as %v, want pass", record.Cell, verdict.Verdict)
		}
	}
}

func TestRunKillMatrixStopsOnFirstInjectFailure(t *testing.T) {
	driver := NewStaticCellDriver(nil)
	failAt := KillMatrixCell{Stage: StageClassBuiltin, Failure: FailureKindPodKill}
	driver.FailAt(failAt, errors.New("injection failed"))

	_, err := RunKillMatrix(context.Background(), driver, "run-1")
	if err == nil {
		t.Fatal("RunKillMatrix should have failed on the first cell")
	}
}

func TestRunKillMatrixRequiresDriver(t *testing.T) {
	if _, err := RunKillMatrix(context.Background(), nil, "run-1"); err == nil {
		t.Fatal("RunKillMatrix(nil driver) should fail — topology-pending, no implementation exists yet")
	}
}
