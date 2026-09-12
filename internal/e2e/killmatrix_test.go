package e2e

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
		Class: "initial", Number: 1, Status: "failure",
		RetryFailureClass: string(journal.AttemptInfra),
		ErrorCode:         errCode, ErrorClass: string(telemetry.ClassifyError(errCode)),
		Placement: &journal.Placement{
			Runner: "r", Pod: pod, Node: node, OS: "linux",
		},
		Error: &journal.ErrorDetail{Code: errCode},
	}
}

func TestClassifyCellResultPass(t *testing.T) {
	successor := placedAttempt(2, string(journal.AttemptInfra), "pod-2", "node-b", "linux")
	successor.Status = "success"
	record := CellInjectionRecord{
		RunID:                    "run-1",
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
	successor.Status = "success"
	interrupted := infraAttempt("pod-1", "node-a", telemetry.ErrCodeInfraNet)
	interrupted.RetryFailureClass = string(journal.AttemptPolicy) // a work-budget charge is an outcome, not the start class
	record := CellInjectionRecord{
		RunID:                    "run-1",
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
	successor.Status = "success"
	record := CellInjectionRecord{
		RunID:                    "run-1",
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
	successor.Status = "success"
	record := CellInjectionRecord{
		RunID:                    "run-1",
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
	successor.Status = "success"
	records := make(map[KillMatrixCell]CellInjectionRecord, 6)
	for _, cell := range KillMatrix() {
		target := "pod-1"
		if cell.Failure == FailureKindNodeKill {
			target = "node-a"
		}
		records[cell] = CellInjectionRecord{
			InjectedAt:               time.Now(),
			InjectedTarget:           target,
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

func TestKillMatrixRejectsMissingTypedCauseAndPolicySuccessor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CellInjectionRecord)
	}{
		{"message-only", func(r *CellInjectionRecord) {
			r.InterruptedAttempt.ErrorCode = "executor_error"
			r.InterruptedAttempt.ErrorClass = "executor"
			r.InterruptedAttempt.Error.Message = "GoobersInfrastructureFailure"
		}},
		{"missing-class", func(r *CellInjectionRecord) { r.InterruptedAttempt.ErrorClass = "" }},
		{"policy-successor", func(r *CellInjectionRecord) { r.SuccessorAttempt.Class = "policy" }},
		{"no-successor", func(r *CellInjectionRecord) { r.SuccessorAttempt = nil }},
		{"failed-successor", func(r *CellInjectionRecord) { r.SuccessorAttempt.Status = "failure" }},
		{"running-successor", func(r *CellInjectionRecord) { r.SuccessorAttempt.Status = "running" }},
		{"missing-successor-status", func(r *CellInjectionRecord) { r.SuccessorAttempt.Status = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			successor := placedAttempt(2, "infra", "pod-2", "node-b", "linux")
			successor.Status = "success"
			record := CellInjectionRecord{RunID: "run-1", Cell: KillMatrix()[0], InjectedAt: time.Now(), InjectedTarget: "pod-1", InterruptedAttempt: infraAttempt("pod-1", "node-a", telemetry.ErrCodeInfraFailure), SuccessorAttempt: &successor, RunCompletedSuccessfully: true}
			tc.mutate(&record)
			if got := ClassifyCellResult(record); got.Verdict != VerdictFail {
				t.Fatalf("verdict = %s, want fail: %s", got.Verdict, got.Detail)
			}
		})
	}
}

func TestKillMatrixRejectsUnboundInjectionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CellInjectionRecord)
	}{
		{"missing-run", func(r *CellInjectionRecord) { r.RunID = "" }},
		{"blank-run", func(r *CellInjectionRecord) { r.RunID = "  " }},
		{"unknown-stage", func(r *CellInjectionRecord) { r.Cell.Stage = "other" }},
		{"unknown-failure", func(r *CellInjectionRecord) { r.Cell.Failure = "other" }},
		{"different-pod", func(r *CellInjectionRecord) { r.InjectedTarget = "unrelated-pod" }},
		{"pod-named-for-node-kill", func(r *CellInjectionRecord) { r.Cell.Failure = FailureKindNodeKill }},
		{"different-node", func(r *CellInjectionRecord) {
			r.Cell.Failure = FailureKindNodeKill
			r.InjectedTarget = "unrelated-node"
		}},
		{"missing-node", func(r *CellInjectionRecord) {
			r.Cell.Failure = FailureKindNodeKill
			r.InjectedTarget = "node-a"
			r.InterruptedAttempt.Placement.Node = ""
		}},
		{"missing-placement", func(r *CellInjectionRecord) { r.InterruptedAttempt.Placement = nil }},
		{"missing-target", func(r *CellInjectionRecord) { r.InjectedTarget = "" }},
		{"missing-time", func(r *CellInjectionRecord) { r.InjectedAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			successor := placedAttempt(2, "infra", "pod-2", "node-b", "linux")
			successor.Status = "success"
			record := CellInjectionRecord{RunID: "run-1", Cell: KillMatrix()[0], InjectedAt: time.Now(), InjectedTarget: "pod-1", InterruptedAttempt: infraAttempt("pod-1", "node-a", telemetry.ErrCodeInfraFailure), SuccessorAttempt: &successor, RunCompletedSuccessfully: true}
			tc.mutate(&record)
			if got := ClassifyCellResult(record); got.Verdict != VerdictInvalid {
				t.Fatalf("verdict = %s, want invalid evidence: %s", got.Verdict, got.Detail)
			}
		})
	}
}
