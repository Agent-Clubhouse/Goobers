package main

import (
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildExecutionEpochKeepsAcceptedPinsAndFencesOldAuthority(t *testing.T) {
	f := actualChildLaunchFixture(t)
	original := publishInterruptedChild(t, f)
	q := f.service.queue
	c, err := q.GetChild(t.Context(), f.submission.Child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	result := triggerqueue.ChildResult{Receipt: []byte("trusted stopped-writer result")}
	result.ReceiptDigest = journal.Digest(result.Receipt)
	if err = q.KeepChildResult(t.Context(), c, result); err != nil {
		t.Fatal(err)
	}
	if err = q.SetChildState(t.Context(), c.Identity, triggerqueue.ChildStateUpdate{Expected: c.State, State: triggerqueue.ChildFailed, ResultRef: result.ReceiptDigest}, time.Now()); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"selected":"guidance"}`)
	e, _, err := q.BeginChildRestart(t.Context(), triggerqueue.ChildRestartRequest{Identity: c.Identity, RunID: "1234567890abcdef1234567890abcdef", SourceRunID: original.RunID, SourceTerminalSeq: 42, SourceResultRef: result.ReceiptDigest, Actor: "issuer:human", Stage: "work", Plan: raw, PlanDigest: journal.Digest(raw)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id := original
	lineage := *original.Child
	id.Child = &lineage
	id.RunID = e.RunID
	id.ContinuedFromRunID = e.SourceRunID
	id.SourceTerminalSeq = e.SourceTerminalSeq
	id.Operator = e.Actor
	id.RequestedTarget = e.Stage
	lineage.ExecutionEpoch = e.Epoch
	lineage.PriorResultRef = e.SourceResultRef
	lineage.RestartDigest = e.RequestDigest
	ref, err := retainedChildExecutionRef(t.Context(), q, id, true)
	if err != nil || ref.Child.RunID != original.RunID || ref.Child.ActiveRunID() != id.RunID || ref.Lineage != lineage {
		t.Fatal(ref, err)
	}
	if _, err = retainedChildExecutionRef(t.Context(), q, original, true); err == nil {
		t.Fatal("old execution retained effect authority")
	}
	if _, err = retainedChildExecutionRef(t.Context(), q, original, false); err != nil {
		t.Fatal("immutable original custody unavailable", err)
	}
	for _, mode := range []string{"actor", "result", "request", "source", "gaggle"} {
		t.Run(mode, func(t *testing.T) {
			changed := id
			child := *id.Child
			changed.Child = &child
			switch mode {
			case "actor":
				changed.Operator = "other"
			case "result":
				child.PriorResultRef = journal.Digest([]byte("other"))
			case "request":
				child.RestartDigest = journal.Digest([]byte("other"))
			case "source":
				changed.SourceTerminalSeq++
			case "gaggle":
				changed.Gaggle = "another"
			}
			if _, err := retainedChildExecutionRef(t.Context(), q, changed, true); err == nil {
				t.Fatal("changed execution authority accepted")
			}
		})
	}
	if err = q.FenceChildParent(t.Context(), c.Identity.ChildParent, "human", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = retainedChildExecutionRef(t.Context(), q, id, true); err == nil {
		t.Fatal("cancelled parent permitted epoch")
	}
	if _, err = retainedChildExecutionRef(t.Context(), q, id, false); err != nil {
		t.Fatal("cancel erased immutable custody", err)
	}
}
