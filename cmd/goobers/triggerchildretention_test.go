package main

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Retention is reached through the daemon's actual sweep even when no scheduler
// is attached. More than one batch proves the production caller is bounded.
func TestDurableTriggerDrainPrunesChildCustodyWithoutScheduler(t *testing.T) {
	dispatch := newDaemonTriggerService()
	path := filepath.Join(t.TempDir(), "accepted.db")
	s := acceptedService(t, path, dispatch)
	acceptedAt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	now := acceptedAt
	dispatch.now = func() time.Time { return now }
	var children []triggerqueue.ChildRecord
	for n := range 105 {
		identity := triggerqueue.ChildIdentity{
			ChildParent:     triggerqueue.ChildParent{Gaggle: "own", ParentRunID: fmt.Sprintf("parent-%d", n)},
			StageOccurrence: "stage", InvocationKey: "call",
		}
		source := []byte(fmt.Sprintf("proposal-%d", n))
		proposal := &triggerqueue.ChildProposal{Source: source, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(source))}
		c, _, err := s.queue.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: identity, Actor: "parent-stage", Payload: []byte("start-envelope"), MaxChildren: 1, Proposal: proposal}, acceptedAt)
		if err != nil {
			t.Fatal(err)
		}
		resultRef := "result:" + c.ChildID
		if err := s.queue.SetChildState(t.Context(), identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: resultRef}, acceptedAt); err != nil {
			t.Fatal(err)
		}
		if err := s.queue.AcknowledgeChild(t.Context(), identity, resultRef, acceptedAt); err != nil {
			t.Fatal(err)
		}
		if err := s.queue.MarkChildParentSettled(t.Context(), identity.ChildParent, acceptedAt); err != nil {
			t.Fatal(err)
		}
		children = append(children, c)
	}
	activeIdentity := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "own", ParentRunID: "active-parent"}, StageOccurrence: "stage", InvocationKey: "call"}
	active, _, err := s.queue.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: activeIdentity, Actor: "parent-stage", Payload: []byte("start-envelope"), MaxChildren: 1}, acceptedAt)
	if err != nil {
		t.Fatal(err)
	}
	now = acceptedAt.Add(triggerqueue.ChildRetention)
	if err := s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	tombstones := 0
	for _, c := range children {
		got, err := s.queue.GetChild(t.Context(), c.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if !got.TombstonedAt.IsZero() {
			tombstones++
		}
	}
	if tombstones != 100 {
		t.Fatalf("one production pass tombstoned %d, want bounded 100", tombstones)
	}
	assertProposalCount := func(want int) {
		t.Helper()
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM child_proposals`).Scan(&count); err != nil || count != want {
			t.Fatalf("production artifact retention count=%d want=%d error=%v", count, want, err)
		}
	}
	assertProposalCount(5)
	if err := s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertProposalCount(0)
	for _, c := range children {
		got, err := s.queue.GetChild(t.Context(), c.Identity)
		if err != nil || got.TombstonedAt.IsZero() {
			t.Fatalf("second pass did not drain retained history: %+v, %v", got, err)
		}
		if _, err := s.queue.ForRun(t.Context(), c.RunID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("terminal receipt retained past tombstone: %v", err)
		}
	}
	now = now.Add(triggerqueue.ChildTombstoneRetention)
	for range 4 {
		if err := s.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range children {
		if _, err := s.queue.GetChild(t.Context(), c.Identity); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expired lineage retained: %v", err)
		}
	}
	got, err := s.queue.GetChild(t.Context(), activeIdentity)
	if err != nil || got.State != triggerqueue.ChildQueued || !got.TombstonedAt.IsZero() {
		t.Fatalf("active child implicitly expired: %+v, %v", got, err)
	}
	if receipt, err := s.queue.ForRun(t.Context(), active.RunID); err != nil || receipt.State != triggerqueue.Accepted {
		t.Fatalf("active custody lost: %+v, %v", receipt, err)
	}
}

func TestDurableTriggerDrainDoesNotDispatchChildAsCatalogWorkflow(t *testing.T) {
	dispatch := newDaemonTriggerService()
	s := acceptedService(t, filepath.Join(t.TempDir(), "accepted.db"), dispatch)
	starter := &acceptedRunIDStarter{ids: make(chan string, 1)}
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{{Gaggle: "own", Workflow: "impl", Starter: starter}}, nil)
	dispatch.AttachScheduler(scheduler)
	dispatch.AttachDispatchContext(t.Context())
	identity := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "own", ParentRunID: "parent"}, StageOccurrence: "stage", InvocationKey: "call"}
	// Even a hybrid payload must not reach a matching ordinary catalog entry.
	child, _, err := s.queue.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: identity, Actor: "parent-stage", Payload: []byte(`{"kind":"child-workflow","request":{"gaggle":"own","workflow":"impl"}}`), MaxChildren: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	scheduler.Wait()
	select {
	case id := <-starter.ids:
		t.Fatalf("child incorrectly ran catalog workflow as %s", id)
	default:
	}
	receipt, err := s.queue.ForRun(t.Context(), child.RunID)
	if err != nil || receipt.State != triggerqueue.Accepted {
		t.Fatalf("child custody claimed without launcher: %+v, %v", receipt, err)
	}
}
