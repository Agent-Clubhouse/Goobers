package triggerqueue

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func reservedChildRequest(parent string) ChildAcceptance {
	req := childRequest(parent, "stage", "key")
	req.Proposal = &ChildProposal{Source: []byte("retained source"), Digest: "sha256:" + childDigest([]byte("retained source"))}
	return req
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
