package triggerqueue

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestActiveChildRecoveryPagesBoundedRetainedCustody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	s := openTestStore(t, path)
	want := map[string]ChildRecord{}
	for i := range 105 {
		req := childRequest("parent", fmt.Sprintf("occurrence-%03d", i), "call")
		if i%2 == 0 {
			req.Identity.Gaggle = "other"
		}
		c := acceptChildTest(t, s, req, childTestTime)
		if i == 0 {
			failChildTest(t, s, c, childTestTime, false)
		} else {
			want[c.ChildID] = c
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	var cursor string
	seen := map[string]bool{}
	for page := 0; page < 2; page++ {
		got, err := s.ActiveChildren(t.Context(), cursor, 100)
		if err != nil {
			t.Fatal(err)
		}
		if (page == 0 && len(got) != 100) || (page == 1 && len(got) != 4) {
			t.Fatalf("page%d len=%d", page, len(got))
		}
		for _, c := range got {
			expected, ok := want[c.ChildID]
			if !ok || seen[c.ChildID] || c.Identity != expected.Identity {
				t.Fatalf("duplicate/lost scope: %+v", c)
			}
			seen[c.ChildID] = true
			cursor = c.ChildID
			receipt, err := s.ChildStart(t.Context(), c.Identity)
			if err != nil || receipt.ID != c.AcceptanceID || receipt.Key != c.StartKey {
				t.Fatalf("start bridge=%+v %v", receipt, err)
			}
		}
	}
	if got, err := s.ActiveChildren(t.Context(), cursor, 100); err != nil || len(got) != 0 {
		t.Fatalf("end page=%d %v", len(got), err)
	}
	for _, limit := range []int{0, 101} {
		if _, err := s.ActiveChildren(t.Context(), "", limit); err == nil {
			t.Fatal("unbounded recovery scan")
		}
	}
	absent := childRequest("not-parent", "not-occurrence", "call").Identity
	if _, err := s.ChildStart(t.Context(), absent); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unexpected cross-identity start %v", err)
	}
}
