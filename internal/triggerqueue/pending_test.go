package triggerqueue

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestPendingCursorVisitsHeldRecordsWithEqualAcceptanceTime(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	now := time.Now().UTC()
	for index := range 103 {
		if _, _, err = s.Accept(t.Context(), fmt.Sprint(index), "human", []byte(`{}`), now); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.Pending(t.Context(), 100)
	if err != nil || len(first) != 100 {
		t.Fatal(len(first), err)
	}
	cursor := PendingCursor{AcceptedAt: first[99].AcceptedAt, ID: first[99].ID}
	if err = s.BeginDispatch(t.Context(), cursor.ID); err != nil {
		t.Fatal(err)
	}
	next, err := s.PendingAfter(t.Context(), cursor, 100)
	if err != nil || len(next) != 3 {
		t.Fatal(len(next), err)
	}
	seen := map[string]bool{}
	for _, r := range first {
		seen[r.ID] = true
	}
	for _, r := range next {
		if seen[r.ID] {
			t.Fatal("cursor repeated held record")
		}
		seen[r.ID] = true
	}
	last := PendingCursor{AcceptedAt: next[2].AcceptedAt, ID: next[2].ID}
	empty, err := s.PendingAfter(t.Context(), last, 100)
	if err != nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
	wrapped, err := s.PendingAfter(t.Context(), PendingCursor{}, 100)
	if err != nil || len(wrapped) != 100 || wrapped[0].ID != first[0].ID {
		t.Fatal("wrap changed custody", err)
	}
}
