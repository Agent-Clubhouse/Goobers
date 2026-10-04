package triggerqueue

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestChildRetentionPinsReceiptAndUnsettledFamily(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	req := childRequest("parent", "stage", "call")
	c := acceptChildTest(t, s, req, childTestTime)
	failChildTest(t, s, c, childTestTime, true)
	// Ordinary seven-day receipt pruning must respect the child dependency.
	acceptTest(t, s, "ordinary", childTestTime.Add(100*24*time.Hour))
	if _, err := s.Get(t.Context(), c.AcceptanceID, req.Actor); err != nil {
		t.Fatalf("ordinary pruning erased child receipt: %v", err)
	}
	if result, err := s.PruneChildren(t.Context(), childTestTime.Add(100*24*time.Hour), 100); err != nil || result.total() != 0 {
		t.Fatalf("unsettled parent pruned: %+v, %v", result, err)
	}
	settledAt := childTestTime.Add(100 * 24 * time.Hour)
	if err := s.MarkChildParentSettled(t.Context(), req.Identity.ChildParent, settledAt); err != nil {
		t.Fatal(err)
	}
	if result, err := s.PruneChildren(t.Context(), settledAt.Add(ChildRetention-time.Nanosecond), 100); err != nil || result.total() != 0 {
		t.Fatalf("early prune: %+v, %v", result, err)
	}
	tombstoneAt := settledAt.Add(ChildRetention)
	result, err := s.PruneChildren(t.Context(), tombstoneAt, 100)
	if err != nil || result.Tombstoned != 1 || result.total() != 1 {
		t.Fatalf("tombstone=%+v, %v", result, err)
	}
	c, duplicate, err := s.AcceptChild(t.Context(), req, tombstoneAt)
	if err != nil || !duplicate || c.TombstonedAt.IsZero() || c.ResultRef != "" || c.WorkspaceRef != "" {
		t.Fatalf("expired retry=%+v, %v, %v", c, duplicate, err)
	}
	req.Payload = []byte("different")
	if _, _, err := s.AcceptChild(t.Context(), req, tombstoneAt); !errors.Is(err, ErrConflict) {
		t.Fatalf("tombstone lost payload conflict: %v", err)
	}
	if _, err := s.Get(t.Context(), c.AcceptanceID, req.Actor); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("receipt not released at tombstone: %v", err)
	}
	if result, err := s.PruneChildren(t.Context(), tombstoneAt.Add(ChildTombstoneRetention-time.Nanosecond), 100); err != nil || result.total() != 0 {
		t.Fatalf("early tombstone expiry: %+v, %v", result, err)
	}
	result, err = s.PruneChildren(t.Context(), tombstoneAt.Add(ChildTombstoneRetention), 100)
	if err != nil || result.Deleted != 1 || result.OccurrencesDeleted != 1 || result.ParentsDeleted != 1 {
		t.Fatalf("expiry=%+v, %v", result, err)
	}
	for _, table := range []string{"child_lineages", "child_occurrences", "child_parents"} {
		if n := childTableCount(t, s, table); n != 0 {
			t.Fatalf("%s retained %d rows", table, n)
		}
	}
}

func TestChildRetentionPinsUnacknowledgedSiblingsAndUncertainDispatch(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	a := acceptChildTest(t, s, childRequest("parent", "a", "call"), childTestTime)
	b := acceptChildTest(t, s, childRequest("parent", "b", "call"), childTestTime)
	failChildTest(t, s, a, childTestTime, true)
	if err := s.BeginDispatch(t.Context(), b.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	failChildTest(t, s, b, childTestTime, false)
	if err := s.MarkChildParentSettled(t.Context(), a.Identity.ChildParent, childTestTime); err != nil {
		t.Fatal(err)
	}
	later := childTestTime.Add(100 * 24 * time.Hour)
	if result, err := s.PruneChildren(t.Context(), later, 100); err != nil || result.total() != 0 {
		t.Fatalf("unacked family not pinned: %+v, %v", result, err)
	}
	if err := s.AcknowledgeChild(t.Context(), b.Identity, "result:"+b.ChildID, childTestTime); err != nil {
		t.Fatal(err)
	}
	result, err := s.PruneChildren(t.Context(), later, 100)
	if err != nil || result.Tombstoned != 1 {
		t.Fatalf("settled sibling prune: %+v, %v", result, err)
	}
	b, err = s.GetChild(t.Context(), b.Identity)
	if err != nil || !b.TombstonedAt.IsZero() {
		t.Fatalf("uncertain dispatch custody erased: %+v, %v", b, err)
	}
	// Recovery proves no process started, and a terminal child must never restart.
	if err := s.RetryUnstarted(t.Context(), b.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.ForRun(t.Context(), b.RunID)
	if err != nil || receipt.State != Rejected {
		t.Fatalf("terminal retry resurrected: %+v, %v", receipt, err)
	}
	result, err = s.PruneChildren(t.Context(), later, 100)
	if err != nil || result.Tombstoned != 1 {
		t.Fatalf("reconciled prune: %+v, %v", result, err)
	}
}

func TestChildRetentionPinsDescendantCustody(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	child := acceptChildTest(t, s, childRequest("parent", "stage", "call"), childTestTime)
	grandchild := acceptChildTest(t, s, childRequest(child.RunID, "nested", "call"), childTestTime)
	failChildTest(t, s, child, childTestTime, true)
	if err := s.MarkChildParentSettled(t.Context(), child.Identity.ChildParent, childTestTime); err != nil {
		t.Fatal(err)
	}
	if result, err := s.PruneChildren(t.Context(), childTestTime.Add(100*24*time.Hour), 100); err != nil || result.total() != 0 {
		t.Fatalf("active descendant lost ancestor custody: %+v, %v", result, err)
	}
	failChildTest(t, s, grandchild, childTestTime, true)
	if err := s.MarkChildParentSettled(t.Context(), grandchild.Identity.ChildParent, childTestTime); err != nil {
		t.Fatal(err)
	}
	later := childTestTime.Add(100 * 24 * time.Hour)
	if result, err := s.PruneChildren(t.Context(), later, 100); err != nil || result.Tombstoned != 1 {
		t.Fatalf("descendant prune=%+v, %v", result, err)
	}
	child, err := s.GetChild(t.Context(), child.Identity)
	if err != nil || !child.TombstonedAt.IsZero() {
		t.Fatalf("retained descendant lost ancestor: %+v, %v", child, err)
	}
	if _, err := s.PruneChildren(t.Context(), later.Add(ChildTombstoneRetention), 100); err != nil {
		t.Fatal(err)
	}
	if result, err := s.PruneChildren(t.Context(), later.Add(ChildTombstoneRetention), 100); err != nil || result.Tombstoned != 1 {
		t.Fatalf("ancestor not released after dependency expiry: %+v, %v", result, err)
	}
}

func TestChildPrunerBoundsGrowthAndSharesPassBudget(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	for n := range 9 {
		req := childRequest(fmt.Sprintf("parent-%d", n), "stage", "call")
		c := acceptChildTest(t, s, req, childTestTime)
		failChildTest(t, s, c, childTestTime, true)
		if err := s.MarkChildParentSettled(t.Context(), req.Identity.ChildParent, childTestTime); err != nil {
			t.Fatal(err)
		}
	}
	later := childTestTime.Add(ChildRetention)
	for range 3 {
		result, err := s.PruneChildren(t.Context(), later, 3)
		if err != nil || result.Tombstoned != 3 || result.total() != 3 {
			t.Fatalf("bounded tombstoning=%+v, %v", result, err)
		}
	}
	if n := childTableCount(t, s, "triggers"); n != 0 {
		t.Fatalf("full receipts remaining=%d", n)
	}
	for n := range 9 {
		result, err := s.PruneChildren(t.Context(), later.Add(ChildTombstoneRetention), 3)
		if err != nil || result.total() != 3 {
			t.Fatalf("bounded expiry pass %d=%+v, %v", n, result, err)
		}
	}
	for _, table := range []string{"child_lineages", "child_occurrences", "child_parents"} {
		if n := childTableCount(t, s, table); n != 0 {
			t.Fatalf("growth not bounded: %s=%d", table, n)
		}
	}
	// Empty cancellation fences also have a production-compatible lifecycle.
	parent := ChildParent{Gaggle: "own", ParentRunID: "empty-parent"}
	if err := s.FenceChildParent(t.Context(), parent, "human", later); err != nil {
		t.Fatal(err)
	}
	if result, err := s.PruneChildren(t.Context(), later.Add(100*24*time.Hour), 3); err != nil || result.total() != 0 {
		t.Fatalf("unsettled empty fence expired: %+v, %v", result, err)
	}
	if err := s.MarkChildParentSettled(t.Context(), parent, later); err != nil {
		t.Fatal(err)
	}
	if result, err := s.PruneChildren(t.Context(), later.Add(ChildRetention), 3); err != nil || result.ParentsDeleted != 1 {
		t.Fatalf("settled empty fence not pruned: %+v, %v", result, err)
	}
}
