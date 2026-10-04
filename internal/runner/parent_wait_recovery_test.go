package runner

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/worktree"
)

func TestRecoveredAcceptedWaitPreservesOriginalAccounting(t *testing.T) {
	for _, mode := range []string{"normal", "missing-accounting", "new-owner", "terminal"} {
		t.Run(mode, func(t *testing.T) {
			_, run, frame := childOriginRuntime(t, &childOriginGoober{})
			totals := newStageUsageTotals()
			accumulateStageUsage(totals, map[string]float64{telemetry.AttrUsageCostUSD: 2})
			if mode == "missing-accounting" {
				if err := frame.recordTaskStarted(1, ""); err != nil {
					t.Fatal(err)
				}
			} else if err := frame.recordTaskStartedWithRecovery(1, "", 0, 1, totals); err != nil {
				t.Fatal(err)
			}
			env := apiv1.InvocationEnvelope{RunID: frame.in.RunID, Gaggle: frame.in.Gaggle, ChildWorkflowOrigin: frame.childOrigin, InstructionAddendum: "keep guidance"}
			request := ChildHandoffRequest{Gaggle: "web", ParentRunID: frame.in.RunID, Action: "wait", RequestID: journal.Digest([]byte("request")), ChildRunID: "child", AcceptanceID: "trigger-child", InvocationKey: "work", SourceDigest: journal.Digest([]byte("source")), Origin: *frame.childOrigin}
			custody := ContainedParentWorkspaceCustody{Version: 1, Origin: frame.childOrigin, Workspace: worktree.StageCustody{OwnerRunID: frame.in.RunID}}
			scope := journal.Event{Stage: frame.t.Name, Attempt: 1, Seq: run.Seq()}
			if err := RecordContainedParentRecovery(run, scope, journal.Digest([]byte("contract")), custody, env, nil, map[string]float64{telemetry.AttrUsageCostUSD: 3}); err != nil {
				t.Fatal(err)
			}
			if mode == "new-owner" {
				if err := frame.recordTaskStartedWithRecovery(2, journal.AttemptInfra, 0, 1, totals); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "terminal" {
				if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
					t.Fatal(err)
				}
			}
			before := run.Seq()
			err := RecoverContainedParentWait(run, request)
			if mode == "missing-accounting" || mode == "new-owner" {
				if err == nil || run.Seq() != before {
					t.Fatal("invalid recovery mutated wait", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			reader, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			record, marker, err := pendingChildWait(events)
			if mode == "terminal" {
				if record != nil || run.Seq() != before {
					t.Fatal("terminal parent silently reopened")
				}
				return
			}
			if err != nil || record == nil || record.PolicyAttempts != 0 || record.InfrastructureFailures != 1 || record.CostUSD != "5" || record.Request.Origin != *frame.childOrigin || record.InstructionAddendum != "keep guidance" || marker.Attempt != 1 {
				t.Fatal(record, marker, err)
			}
			before = run.Seq()
			if err = RecoverContainedParentWait(run, request); err != nil || run.Seq() != before {
				t.Fatal("recovery replay changed marker", err)
			}
		})
	}
}
