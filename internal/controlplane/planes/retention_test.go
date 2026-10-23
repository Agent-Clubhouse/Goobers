package planes

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestDurableTriggerDrainPrunesChildCustodyWithoutScheduler(t *testing.T) {
	trigger := NewTriggerService()
	path := filepath.Join(t.TempDir(), "accepted.db")
	service, err := NewDurableTriggerService(path, trigger, durableBackendStub{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	service.pruneBudget = time.Minute
	acceptedAt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	now := acceptedAt
	trigger.SetClock(func() time.Time { return now })
	var children []triggerqueue.ChildRecord
	for n := range 105 {
		identity := triggerqueue.ChildIdentity{
			ChildParent:     triggerqueue.ChildParent{Gaggle: "own", ParentRunID: fmt.Sprintf("parent-%d", n)},
			StageOccurrence: "stage", InvocationKey: "call",
		}
		source := []byte(fmt.Sprintf("proposal-%d", n))
		proposal := &triggerqueue.ChildProposal{Source: source, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(source))}
		child, _, err := service.queue.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: identity, Actor: "parent-stage", Payload: []byte("start-envelope"), MaxChildren: 1, Proposal: proposal}, acceptedAt)
		if err != nil {
			t.Fatal(err)
		}
		resultRef := "result:" + child.ChildID
		if err := service.queue.SetChildState(t.Context(), identity, triggerqueue.ChildStateUpdate{Expected: triggerqueue.ChildQueued, State: triggerqueue.ChildFailed, ResultRef: resultRef}, acceptedAt); err != nil {
			t.Fatal(err)
		}
		if err := service.queue.AcknowledgeChild(t.Context(), identity, resultRef, acceptedAt); err != nil {
			t.Fatal(err)
		}
		if err := service.queue.MarkChildParentSettled(t.Context(), identity.ChildParent, acceptedAt); err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
	activeIdentity := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "own", ParentRunID: "active-parent"}, StageOccurrence: "stage", InvocationKey: "call"}
	active, _, err := service.queue.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: activeIdentity, Actor: "parent-stage", Payload: []byte("start-envelope"), MaxChildren: 1}, acceptedAt)
	if err != nil {
		t.Fatal(err)
	}
	now = acceptedAt.Add(triggerqueue.ChildRetention)
	if err := service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	tombstones := 0
	for _, child := range children {
		got, err := service.queue.GetChild(t.Context(), child.Identity)
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
	if err := service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertProposalCount(0)
	for _, child := range children {
		got, err := service.queue.GetChild(t.Context(), child.Identity)
		if err != nil || got.TombstonedAt.IsZero() {
			t.Fatalf("second pass did not drain retained history: %+v, %v", got, err)
		}
		if _, err := service.queue.ForRun(t.Context(), child.RunID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("terminal receipt retained past tombstone: %v", err)
		}
	}
	now = now.Add(triggerqueue.ChildTombstoneRetention)
	for range 4 {
		if err := service.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, child := range children {
		if _, err := service.queue.GetChild(t.Context(), child.Identity); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expired lineage retained: %v", err)
		}
	}
	got, err := service.queue.GetChild(t.Context(), activeIdentity)
	if err != nil || got.State != triggerqueue.ChildQueued || !got.TombstonedAt.IsZero() {
		t.Fatalf("active child implicitly expired: %+v, %v", got, err)
	}
	if receipt, err := service.queue.ForRun(t.Context(), active.RunID); err != nil || receipt.State != triggerqueue.Accepted {
		t.Fatalf("active custody lost: %+v, %v", receipt, err)
	}
}
