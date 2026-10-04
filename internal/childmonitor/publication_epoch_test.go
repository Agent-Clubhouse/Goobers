package childmonitor

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func restartMonitorPublication(t *testing.T, q *triggerqueue.Store, layout instance.Layout, child triggerqueue.ChildRecord, id journal.RunIdentity) (triggerqueue.ChildRecord, journal.RunIdentity) {
	t.Helper()
	r := triggerqueue.ChildResult{Receipt: []byte("sealed original result")}
	r.ReceiptDigest = journal.Digest(r.Receipt)
	if err := q.KeepChildResult(t.Context(), child, r); err != nil {
		t.Fatal(err)
	}
	if err := q.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{Expected: child.State, State: triggerqueue.ChildFailed, ResultRef: r.ReceiptDigest}, time.Now()); err != nil {
		t.Fatal(err)
	}
	dir, err := layout.FindRunDir(id.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	plan := []byte("retained human plan")
	e, _, err := q.BeginChildRestart(t.Context(), triggerqueue.ChildRestartRequest{Identity: child.Identity, RunID: strings.Repeat("f", 32), SourceRunID: id.RunID, SourceTerminalSeq: events[len(events)-1].Seq, SourceResultRef: r.ReceiptDigest, Stage: "repair", Actor: "issuer:human", Plan: plan, PlanDigest: journal.Digest(plan)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	lineage := *id.Child
	lineage.ExecutionEpoch, lineage.PriorResultRef, lineage.RestartDigest = e.Epoch, e.SourceResultRef, e.RequestDigest
	next, err := journal.CreateContinuation(layout.ForGaggle(id.Gaggle).RunsDir(), journal.ContinuationRequest{RunID: e.RunID, SourceRunID: id.RunID, ExpectedTerminalSeq: e.SourceTerminalSeq, Operator: e.Actor, Target: e.Stage, ChildContinuation: &lineage})
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Append(journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err = next.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err = journal.OpenReadOnly(next.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err = reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	child, err = q.GetChild(t.Context(), child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	return child, id
}
