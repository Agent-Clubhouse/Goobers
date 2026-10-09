package runner

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

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
