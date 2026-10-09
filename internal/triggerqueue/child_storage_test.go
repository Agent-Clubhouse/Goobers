package triggerqueue

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reservedChildRequest(parent string) ChildAcceptance {
	req := childRequest(parent, "stage", "key")
	source := []byte("retained source")
	req.Proposal = &ChildProposal{Source: source, Digest: "sha256:" + childDigest(source)}
	return req
}

func TestChildStorageReservationsFenceOrdinaryAndChildIntakeAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	s := openTestStore(t, path)
	var first ChildRecord
	for i := range 5 {
		child := acceptChildTest(t, s, reservedChildRequest(fmt.Sprintf("parent-%d", i)), childTestTime)
		if i == 0 {
			first = child
		}
	}
	// Simulate other retained database content without allocating millions of
	// receipts. Intake must account for both actual bytes and promised custody.
	if _, err := s.db.Exec(`CREATE TABLE test_padding(data BLOB); INSERT INTO test_padding VALUES(zeroblob(52428800));`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	if _, _, err := s.AcceptChild(t.Context(), reservedChildRequest("additional"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatalf("child intake consumed promised completion space: %v", err)
	}
	if _, _, err := s.Accept(t.Context(), "ordinary", "operator", []byte(`{}`), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatalf("ordinary intake consumed promised completion space: %v", err)
	}
	if got, duplicate, err := s.AcceptChild(t.Context(), reservedChildRequest("parent-0"), childTestTime); err != nil || !duplicate || got.ChildID != first.ChildID {
		t.Fatalf("capacity prevented recovery of an existing receipt: %+v %v %v", got, duplicate, err)
	}
	// Existing acknowledgement releases the retained slot and its reservation.
	// Separate completion tests exercise stored workspace and result bytes.
	failChildTest(t, s, first, childTestTime.Add(time.Minute), true)
	if _, duplicate, err := s.Accept(t.Context(), "ordinary", "operator", []byte(`{}`), childTestTime.Add(2*time.Minute)); err != nil || duplicate {
		t.Fatalf("acknowledgement did not release admission capacity: duplicate=%v err=%v", duplicate, err)
	}
}

func childReservedBytes(t *testing.T, s *Store, id string) int {
	t.Helper()
	var size int
	if err := s.db.QueryRow(`SELECT reserved_bytes FROM child_lineages WHERE child_id=?`, id).Scan(&size); err != nil {
		t.Fatal(err)
	}
	return size
}

func TestChildStorageAdmissionPreservesWorstCaseCompletionAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	s := openTestStore(t, path)
	var children []ChildRecord
	for i := range 5 {
		children = append(children, acceptChildTest(t, s, reservedChildRequest(fmt.Sprintf("parent-%d", i)), childTestTime))
	}
	if childReservedBytes(t, s, children[0].ChildID) != childCompletionAllowance {
		t.Fatal("missing completion reservation")
	}
	// Model retained ordinary receipts occupying space before a later start.
	if _, err := s.db.Exec(`CREATE TABLE test_padding(data BLOB); INSERT INTO test_padding VALUES(zeroblob(31457280));`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptChild(t.Context(), reservedChildRequest("too-many"), childTestTime); !errors.Is(err, ErrFull) {
		t.Fatal("intake spent completion space", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	child := children[0]
	snapshot := childSnapshotTestValue(strings.Repeat("r", MaxChildSnapshotReceiptBytes), strings.Repeat("b", MaxChildSnapshotBytes))
	if err := s.KeepChildSnapshot(t.Context(), child, snapshot); err != nil {
		t.Fatal("reserved fork could not finish", err)
	}
	if got := childReservedBytes(t, s, child.ChildID); got != childCompletionAllowance-childSnapshotAllowance {
		t.Fatal("fork did not consume own allowance", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	if err := s.KeepChildSnapshot(t.Context(), child, snapshot); err != nil {
		t.Fatal("fork retry after reopen failed", err)
	}
	if got := childReservedBytes(t, s, child.ChildID); got != childCompletionAllowance-childSnapshotAllowance {
		t.Fatal("fork retry double-consumed reservation", got)
	}
	result := childResultValue(strings.Repeat("r", MaxChildSnapshotReceiptBytes), strings.Repeat("b", MaxChildSnapshotBytes))
	if err := s.KeepChildResult(t.Context(), child, result); err != nil {
		t.Fatal("reserved result could not finish", err)
	}
	if got := childReservedBytes(t, s, child.ChildID); got != childRequestAllowance+childPlanAllowance+childDispositionHistoryAllowance {
		t.Fatal("terminal result consumed wrong allowance", got)
	}
	if err := s.KeepChildResult(t.Context(), child, result); err != nil {
		t.Fatal(err)
	}
	if got := childReservedBytes(t, s, child.ChildID); got != childRequestAllowance+childPlanAllowance+childDispositionHistoryAllowance {
		t.Fatal("retry double-consumed reservation", got)
	}
}

func TestChildScratchResultReleasesUnusedForkReservation(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	child := acceptChildTest(t, s, reservedChildRequest("scratch"), childTestTime)
	if err := s.KeepChildResult(t.Context(), child, childResultValue("scratch result", "")); err != nil {
		t.Fatal(err)
	}
	if got := childReservedBytes(t, s, child.ChildID); got != childRequestAllowance+childPlanAllowance+childDispositionHistoryAllowance {
		t.Fatal("unused fork reservation not released", got)
	}
}
