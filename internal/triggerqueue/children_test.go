package triggerqueue

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var childTestTime = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func childRequest(parent, occurrence, key string) ChildAcceptance {
	return ChildAcceptance{
		Identity: ChildIdentity{ChildParent: ChildParent{Gaggle: "own", ParentRunID: parent}, StageOccurrence: occurrence, InvocationKey: key},
		Actor:    "parent-stage", Payload: []byte(`{"request":{"workflow":"child","gaggle":"own"}}`), MaxChildren: 2,
	}
}

func acceptChildTest(t *testing.T, s *Store, req ChildAcceptance, now time.Time) ChildRecord {
	t.Helper()
	c, duplicate, err := s.AcceptChild(t.Context(), req, now)
	if err != nil || duplicate {
		t.Fatalf("AcceptChild = %+v, %v, %v", c, duplicate, err)
	}
	return c
}

func failChildTest(t *testing.T, s *Store, c ChildRecord, now time.Time, ack bool) {
	t.Helper()
	ref := "result:" + c.ChildID
	if err := s.SetChildState(t.Context(), c.Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildFailed, ResultRef: ref}, now); err != nil {
		t.Fatal(err)
	}
	if ack {
		if err := s.AcknowledgeChild(t.Context(), c.Identity, ref, now); err != nil {
			t.Fatal(err)
		}
	}
}

func childTableCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	// Test-owned fixed table names, never caller input.
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestChildAcceptanceAtomicIdentityAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	s := openTestStore(t, path)
	req := childRequest("parent", "stage/branch/visit-1", "call-1")
	c := acceptChildTest(t, s, req, childTestTime)
	if c.Sequence != 1 || c.State != ChildQueued || len(c.RunID) != 32 || c.ChildID != "child-"+c.RunID || c.AcceptanceID != "trigger-"+c.RunID || c.StartKey != childStartKey(req.Identity) {
		t.Fatalf("identity = %+v", c)
	}
	r, err := s.Get(t.Context(), c.AcceptanceID, req.Actor)
	if err != nil || r.Key != c.StartKey || string(r.Payload) != string(req.Payload) || r.State != Accepted {
		t.Fatalf("receipt = %+v, %v", r, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	dup, duplicate, err := s.AcceptChild(t.Context(), req, childTestTime.Add(time.Hour))
	if err != nil || !duplicate || dup != c {
		t.Fatalf("reopened duplicate = %+v, %v, %v", dup, duplicate, err)
	}
	for _, changed := range []ChildAcceptance{func() ChildAcceptance { v := req; v.Payload = []byte("changed"); return v }(), func() ChildAcceptance { v := req; v.Actor = "other"; return v }()} {
		if _, _, err := s.AcceptChild(t.Context(), changed, childTestTime); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed retry = %v", err)
		}
	}
	other := req
	other.Identity.Gaggle = "other"
	c2 := acceptChildTest(t, s, other, childTestTime)
	if c2.StartKey == c.StartKey || c2.ChildID == c.ChildID {
		t.Fatal("gaggles shared identity")
	}
	if err := s.FenceChildParent(t.Context(), req.Identity.ChildParent, "human", childTestTime); err != nil {
		t.Fatal(err)
	}
	duplicateRecord, duplicate, err := s.AcceptChild(t.Context(), req, childTestTime)
	if err != nil || !duplicate || !duplicateRecord.CancellationRequested {
		t.Fatalf("retry after fence = %+v, %v, %v", duplicateRecord, duplicate, err)
	}
	otherRecord, err := s.GetChild(t.Context(), other.Identity)
	if err != nil || otherRecord.CancellationRequested {
		t.Fatalf("other gaggle affected = %+v, %v", otherRecord, err)
	}
}

func TestChildAcceptanceRollbackDoesNotConsumeSlotOrReceipt(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	if _, err := s.db.Exec(`CREATE TRIGGER fail_child_insert BEFORE INSERT ON child_lineages BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	req := childRequest("parent", "stage", "call")
	if _, _, err := s.AcceptChild(t.Context(), req, childTestTime); err == nil {
		t.Fatal("expected injected failure")
	}
	for _, table := range []string{"triggers", "child_lineages", "child_occurrences", "child_parents"} {
		if n := childTableCount(t, s, table); n != 0 {
			t.Fatalf("%s orphaned %d rows", table, n)
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_child_insert`); err != nil {
		t.Fatal(err)
	}
	if c := acceptChildTest(t, s, req, childTestTime); c.Sequence != 1 {
		t.Fatalf("sequence = %d", c.Sequence)
	}
}

func TestChildOccurrenceSlotRaceAndAcknowledgement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	a, b := openTestStore(t, path), openTestStore(t, path)
	start := make(chan struct{})
	type outcome struct {
		child ChildRecord
		err   error
	}
	results := make(chan outcome, 2)
	for i, s := range []*Store{a, b} {
		go func() {
			<-start
			c, _, err := s.AcceptChild(t.Context(), childRequest("parent", "stage", fmt.Sprintf("call-%d", i)), childTestTime)
			results <- outcome{c, err}
		}()
	}
	close(start)
	var winner ChildRecord
	winners, blocked := 0, 0
	for range 2 {
		out := <-results
		switch {
		case out.err == nil:
			winner = out.child
			winners++
		case errors.Is(out.err, ErrChildSlotOccupied):
			blocked++
		default:
			t.Fatal(out.err)
		}
	}
	if winners != 1 || blocked != 1 {
		t.Fatalf("winners=%d blocked=%d", winners, blocked)
	}
	// Other parallel occurrences can progress independently.
	acceptChildTest(t, a, childRequest("parent", "branch-2", "call"), childTestTime)
	failChildTest(t, a, winner, childTestTime, true)
	if err := a.AcknowledgeChild(t.Context(), winner.Identity, "result:"+winner.ChildID, childTestTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	nextReq := childRequest("parent", "stage", "next")
	nextReq.MaxChildren = 32 // Cannot widen the ceiling pinned by the first child.
	next := acceptChildTest(t, b, nextReq, childTestTime.Add(time.Second))
	if next.Sequence != 2 {
		t.Fatalf("sequence=%d", next.Sequence)
	}
	failChildTest(t, a, next, childTestTime.Add(time.Second), false)
	thirdReq := childRequest("parent", "stage", "third")
	thirdReq.MaxChildren = 32
	if _, _, err := a.AcceptChild(t.Context(), thirdReq, childTestTime.Add(time.Second)); !errors.Is(err, ErrChildSlotOccupied) {
		t.Fatalf("terminal but unacked = %v", err)
	}
	if err := a.AcknowledgeChild(t.Context(), next.Identity, "wrong-result", childTestTime.Add(time.Second)); !errors.Is(err, ErrTransition) {
		t.Fatalf("wrong result ack = %v", err)
	}
	if err := a.AcknowledgeChild(t.Context(), next.Identity, "result:"+next.ChildID, childTestTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.AcceptChild(t.Context(), thirdReq, childTestTime.Add(time.Second)); !errors.Is(err, ErrChildLimit) {
		t.Fatalf("ceiling = %v", err)
	}
}

func TestChildStateCASAndDispatchCustody(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	c := acceptChildTest(t, s, childRequest("parent", "stage", "call"), childTestTime)
	update := ChildStateUpdate{Expected: ChildQueued, State: ChildRunning, WorkspaceRef: "workspace:snapshot"}
	if err := s.SetChildState(t.Context(), c.Identity, update, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatalf("running without dispatch claim = %v", err)
	}
	if err := s.BeginDispatch(t.Context(), c.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDispatch(t.Context(), c.AcceptanceID, c.RunID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildState(t.Context(), c.Identity, update, childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildState(t.Context(), c.Identity, update, childTestTime); err != nil {
		t.Fatalf("lost response retry: %v", err)
	}
	if err := s.SetChildState(t.Context(), c.Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildFailed, ResultRef: "result:stale"}, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatalf("stale mutation=%v", err)
	}
	if err := s.SetChildState(t.Context(), c.Identity, ChildStateUpdate{Expected: ChildRunning, State: ChildAwaitingHuman}, childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgeChild(t.Context(), c.Identity, "result:missing", childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatalf("nonterminal ack=%v", err)
	}
	complete := ChildStateUpdate{Expected: ChildAwaitingHuman, State: ChildCompleted, ResultRef: "result:verified", WorkspaceRef: "workspace:snapshot"}
	if err := s.SetChildState(t.Context(), c.Identity, complete, childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildState(t.Context(), c.Identity, complete, childTestTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	complete.ResultRef = "result:different"
	if err := s.SetChildState(t.Context(), c.Identity, complete, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatalf("terminal mutation=%v", err)
	}
}

func TestChildParentFenceRacesAcceptanceAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	a, b := openTestStore(t, path), openTestStore(t, path)
	for n := range 12 {
		req := childRequest(fmt.Sprintf("parent-%d", n), "stage", "call")
		start := make(chan struct{})
		var wg sync.WaitGroup
		var c ChildRecord
		var acceptErr, fenceErr error
		wg.Add(2)
		go func() { defer wg.Done(); <-start; c, _, acceptErr = a.AcceptChild(t.Context(), req, childTestTime) }()
		go func() {
			defer wg.Done()
			<-start
			fenceErr = b.FenceChildParent(t.Context(), req.Identity.ChildParent, "human", childTestTime)
		}()
		close(start)
		wg.Wait()
		if fenceErr != nil {
			t.Fatal(fenceErr)
		}
		if acceptErr == nil {
			r, err := a.Get(t.Context(), c.AcceptanceID, req.Actor)
			if err != nil || r.State != Rejected {
				t.Fatalf("accepted-before-fence receipt=%+v, %v", r, err)
			}
			if err := a.BeginDispatch(t.Context(), c.AcceptanceID); !errors.Is(err, ErrTransition) {
				t.Fatalf("fenced dispatch=%v", err)
			}
		} else if !errors.Is(acceptErr, ErrParentCancelled) {
			t.Fatal(acceptErr)
		}
		req.Identity.InvocationKey = "later"
		if _, _, err := b.AcceptChild(t.Context(), req, childTestTime); !errors.Is(err, ErrParentCancelled) {
			t.Fatalf("post-fence acceptance=%v", err)
		}
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	a = openTestStore(t, path)
	if _, _, err := a.AcceptChild(t.Context(), childRequest("parent-0", "different", "call"), childTestTime); !errors.Is(err, ErrParentCancelled) {
		t.Fatalf("fence lost at reopen: %v", err)
	}
}

func TestChildCancellationOutboxDoesNotClaimStoppedAndFencesRequeue(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	parent := ChildParent{Gaggle: "own", ParentRunID: "parent"}
	c := acceptChildTest(t, s, childRequest("parent", "stage", "call"), childTestTime)
	if err := s.BeginDispatch(t.Context(), c.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := s.FenceChildParent(t.Context(), parent, "human", childTestTime); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get(t.Context(), c.AcceptanceID, "parent-stage")
	if err != nil || r.State != Dispatching {
		t.Fatalf("claimed receipt changed by fence: %+v, %v", r, err)
	}
	outbox, err := s.PendingChildCancellations(t.Context(), parent, "", 1)
	if err != nil || len(outbox) != 1 || outbox[0].State != ChildQueued || !outbox[0].CancellationRequested {
		t.Fatalf("outbox=%+v, %v", outbox, err)
	}
	if err := s.RetryUnstarted(t.Context(), c.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	r, err = s.Get(t.Context(), c.AcceptanceID, "parent-stage")
	if err != nil || r.State != Rejected {
		t.Fatalf("recovery resurrected cancelled start: %+v, %v", r, err)
	}
	if err := s.SetChildState(t.Context(), c.Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildCancelled, ResultRef: "result:cancelled"}, childTestTime); err != nil {
		t.Fatal(err)
	}
	outbox, err = s.PendingChildCancellations(t.Context(), parent, "", 100)
	if err != nil || len(outbox) != 0 {
		t.Fatalf("terminal outbox=%+v, %v", outbox, err)
	}
	if err := s.MarkChildParentSettled(t.Context(), parent, childTestTime); err != nil {
		t.Fatal(err)
	}
	if result, err := s.PruneChildren(t.Context(), childTestTime.Add(365*24*time.Hour), 100); err != nil || result.total() != 0 {
		t.Fatalf("unacknowledged cancel must remain pinned: %+v, %v", result, err)
	}
}

func TestChildIntakeBoundsAndInputValidation(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	bad := []ChildAcceptance{childRequest("parent", "stage", "call"), childRequest("parent", "stage", "call"), childRequest("parent", "stage", "call"), childRequest("parent", "stage", "call")}
	bad[0].MaxChildren = 33
	bad[1].Identity.Gaggle = ""
	bad[2].Payload = make([]byte, MaxPayloadBytes+1)
	bad[3].Identity.InvocationKey = "bad\nkey"
	for _, req := range bad {
		if _, _, err := s.AcceptChild(t.Context(), req, childTestTime); err == nil {
			t.Fatalf("invalid accepted: %+v", req.Identity)
		}
	}
	if n := childTableCount(t, s, "triggers"); n != 0 {
		t.Fatalf("invalid requests left %d receipts", n)
	}
	if _, err := s.db.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<?) INSERT INTO triggers(id,key,actor,payload,state,accepted_ns) SELECT 'seed-'||i,'key-'||i,'test',X'01','accepted',? FROM n`, MaxRecords, childTestTime.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptChild(t.Context(), childRequest("parent", "stage", "call"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatalf("full queue acceptance=%v", err)
	}
	if n := childTableCount(t, s, "child_parents"); n != 0 {
		t.Fatalf("full queue leaked %d parents", n)
	}
	// References are bounded independently from small start envelopes.
	if err := s.SetChildState(t.Context(), childRequest("parent", "stage", "call").Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildFailed, ResultRef: strings.Repeat("x", MaxChildRefBytes+1)}, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatalf("oversize ref=%v", err)
	}
}

func TestChildParentSettlementClosesNewSubmissions(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	req := childRequest("parent", "stage", "call")
	if err := s.MarkChildParentSettled(t.Context(), req.Identity.ChildParent, childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptChild(t.Context(), req, childTestTime); !errors.Is(err, ErrParentSettled) {
		t.Fatalf("settled acceptance=%v", err)
	}
	if _, err := s.GetChild(t.Context(), req.Identity); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("settled parent fabricated child: %v", err)
	}
}

func TestChildParentCapacityPreservesExistingFamilyAndRecovers(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	// Fill the separate cancellation-fence namespace without allocating starts.
	if _, err := s.db.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<?)
 INSERT INTO child_parents(gaggle,parent_run,created_ns) SELECT 'own','parent-'||i,? FROM n`, MaxChildLineages, childTestTime.UnixNano()); err != nil {
		t.Fatal(err)
	}
	acceptChildTest(t, s, childRequest("parent-1", "stage", "call"), childTestTime)
	if _, _, err := s.AcceptChild(t.Context(), childRequest("new-parent", "stage", "call"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatalf("parent capacity = %v", err)
	}
	if n := childTableCount(t, s, "triggers"); n != 1 {
		t.Fatalf("failed family intake leaked receipt: %d", n)
	}
	if err := s.FenceChildParent(t.Context(), ChildParent{Gaggle: "own", ParentRunID: "parent-1"}, "human", childTestTime); err != nil {
		t.Fatalf("capacity prevented existing family cancellation: %v", err)
	}
	if err := s.MarkChildParentSettled(t.Context(), ChildParent{Gaggle: "own", ParentRunID: "parent-2"}, childTestTime); err != nil {
		t.Fatal(err)
	}
	result, err := s.PruneChildren(t.Context(), childTestTime.Add(ChildRetention), 100)
	if err != nil || result.ParentsDeleted != 1 {
		t.Fatalf("family capacity recovery = %+v, %v", result, err)
	}
	acceptChildTest(t, s, childRequest("new-parent", "stage", "call"), childTestTime.Add(ChildRetention))
}
