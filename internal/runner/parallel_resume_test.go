package runner

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestParallelParentRecoveryPreservesReceiptAndMutationRefusal(t *testing.T) {
	for _, mutated := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovered", true: "external-write"}[mutated], func(t *testing.T) {
			_, run, frame := childOriginRuntime(t, &childOriginGoober{})
			writer := &branchJournal{run: run, branch: 1}
			frame.jr = writer
			if err := frame.recordTaskStartedWithRecovery(1, "", 0, 0, newStageUsageTotals()); err != nil {
				t.Fatal(err)
			}
			custody := ContainedParentWorkspaceCustody{Version: 1, Origin: frame.childOrigin, Workspace: worktree.StageCustody{OwnerRunID: frame.in.RunID}}
			env := apiv1.InvocationEnvelope{RunID: frame.in.RunID, ChildWorkflowOrigin: frame.childOrigin}
			if err := RecordContainedParentRecovery(run, journal.Event{Stage: frame.t.Name, Branch: 1, Attempt: 1, Seq: run.Seq()}, journal.Digest([]byte("contract")), custody, env, nil, nil); err != nil {
				t.Fatal(err)
			}
			if mutated {
				if err := writer.Append(journal.Event{Type: journal.EventRefTouched, Stage: frame.t.Name, Attempt: 1, ExternalRef: &journal.ExternalRef{Kind: "pr", ID: "7"}}); err != nil {
					t.Fatal(err)
				}
			}
			reader, err := journal.OpenReadOnly(run.Dir())
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			before := run.Seq()
			restored, err := recoverParallelStage(writer, frame.in.Machine, frame.t.Name, apiv1.ResultEnvelope{}, events)
			if mutated {
				if err == nil || !strings.Contains(err.Error(), "external mutation") {
					t.Fatal("recovered receipt bypassed external-write refusal", err)
				}
			} else if err != nil || !restored.parent || restored.attempt != 2 || restored.accounting == nil {
				t.Fatal("host receipt lost before accepted-child recovery", restored, err)
			}
			if run.Seq() != before {
				t.Fatal("generic interruption consumed recovered custody")
			}
		})
	}
}

func TestParallelChildRecoveryErrorCannotSettleUnresolvedWait(t *testing.T) {
	r, run, frame := childOriginRuntime(t, &childOriginGoober{})
	f := newParallelCapacityFixture(t)
	r.cfg.ChildParentCapacity = f.capacity
	par := newParallelExec(apiv1.Parallel{Name: "fan", Branches: []apiv1.Branch{{Name: "a", Start: frame.t.Name}}})
	if err := run.Append(journal.Event{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: par.completeness()}); err != nil {
		t.Fatal(err)
	}
	frame.jr = &branchJournal{run: run, branch: 1}
	if err := frame.recordTaskStarted(1, ""); err != nil {
		t.Fatal(err)
	}
	record := childWaitRecord{Version: 1, ParentRunID: frame.in.RunID, Request: ChildHandoffRequest{Gaggle: frame.in.Gaggle, ParentRunID: frame.in.RunID, RequestID: journal.Digest([]byte("wait")), Action: "wait", ChildRunID: "child-run", AcceptanceID: "trigger-child-run", InvocationKey: "child", SourceDigest: journal.Digest([]byte("source")), Origin: *frame.childOrigin}}
	wait, err := childWaitEvent(frame.t.Name, 1, "", record)
	if err != nil {
		t.Fatal(err)
	}
	if err := frame.jr.Append(wait); err != nil {
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
	owner, err := r.parallelChildOwner(frame.in, par, events)
	if err != nil {
		t.Fatal(err)
	}
	before := run.Seq()
	err = settleParallelChildBranch(t.Context(), run, par, owner, parallelBranchResult{index: 0, status: journal.BranchFailed})
	if err == nil || !strings.Contains(err.Error(), "unresolved child custody") {
		t.Fatal("failed branch erased its child wait", err)
	}
	if run.Seq() != before || par.branchSnapshot(0).settled {
		t.Fatal("refused settlement changed durable or live branch state")
	}
	events, err = reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	projection, err := journal.ProjectChildWaits(events)
	if err != nil || len(projection.Waits) != 1 {
		t.Fatal("refused settlement corrupted recovery custody", err)
	}
}
