package triggerqueue

import (
	"errors"
	"fmt"
	"path/filepath"
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
	// Writing/consuming actual workspace and result bytes belongs to C03.
	failChildTest(t, s, first, childTestTime.Add(time.Minute), true)
	if _, duplicate, err := s.Accept(t.Context(), "ordinary", "operator", []byte(`{}`), childTestTime.Add(2*time.Minute)); err != nil || duplicate {
		t.Fatalf("acknowledgement did not release admission capacity: duplicate=%v err=%v", duplicate, err)
	}
}
