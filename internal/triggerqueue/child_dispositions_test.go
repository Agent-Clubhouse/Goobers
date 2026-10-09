package triggerqueue

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func dispositionFixture(t *testing.T, s *Store, parent string, at time.Time) (ChildRecord, ChildDispositionRequest) {
	t.Helper()
	req := childRequest(parent, "stage", "key")
	a := childAuthorityFixture(req)
	a.ExpiresAt = at.Add(time.Hour)
	if err := s.BindChildAuthority(t.Context(), a, "", at); err != nil {
		t.Fatal(err)
	}
	req.Authority = &a
	child := acceptChildTest(t, s, req, at)
	r := childResultValue("terminal", "")
	if err := s.KeepChildResult(t.Context(), child, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildState(t.Context(), child.Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildFailed, ResultRef: r.ReceiptDigest}, at); err != nil {
		t.Fatal(err)
	}
	return child, ChildDispositionRequest{Identity: child.Identity, Action: "discard", ResultRef: r.ReceiptDigest, Authority: a}
}

func TestChildDispositionAuthorityIntentAndAcknowledgement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	s := openTestStore(t, path)
	child, request := dispositionFixture(t, s, "parent", childTestTime)
	d, err := s.RequestChildDisposition(t.Context(), request, childTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestChildDisposition(t.Context(), request, childTestTime); err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.Action = "merge"
	if _, err := s.RequestChildDisposition(t.Context(), changed, childTestTime); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := s.CompleteChildDisposition(t.Context(), d, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatal("acknowledged before intent", err)
	}
	if err := s.KeepChildDispositionPlan(t.Context(), d, []byte("exact plan")); err != nil {
		t.Fatal(err)
	}
	if err := s.KeepChildDispositionPlan(t.Context(), d, []byte("replacement")); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	d, err = s.ChildDisposition(t.Context(), child.Identity)
	if err != nil || string(d.Plan) != "exact plan" {
		t.Fatal(d, err)
	}
	if current, err := s.GetChild(t.Context(), child.Identity); err != nil || !current.AcknowledgedAt.IsZero() {
		t.Fatal("intent released slot", err)
	}
	if err := s.CompleteChildDisposition(t.Context(), d, childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteChildDisposition(t.Context(), d, childTestTime); err != nil {
		t.Fatal(err)
	}
	if current, err := s.GetChild(t.Context(), child.Identity); err != nil || current.AcknowledgedAt.IsZero() {
		t.Fatal("verified application did not release slot", err)
	}
	if err := s.RevokeChildAuthority(t.Context(), request.Authority); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestChildDisposition(t.Context(), request, childTestTime); !errors.Is(err, ErrChildAuthorityChanged) {
		t.Fatal("revoked attempt read prior disposition", err)
	}
}

func TestChildDispositionMissingEvidenceIsNotReapplicationPermission(t *testing.T) {
	for _, query := range []string{`DELETE FROM child_dispositions`, `UPDATE child_dispositions SET action='merge'`, `UPDATE child_dispositions SET plan=x'626164'`} {
		t.Run(query, func(t *testing.T) {
			s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
			child, request := dispositionFixture(t, s, "parent", childTestTime)
			d, err := s.RequestChildDisposition(t.Context(), request, childTestTime)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.KeepChildDispositionPlan(t.Context(), d, []byte("plan")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ChildDisposition(t.Context(), child.Identity); !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
			if _, err := s.RequestChildDisposition(t.Context(), request, childTestTime); !errors.Is(err, ErrConflict) {
				t.Fatal("lost evidence repaired", err)
			}
		})
	}
}

func TestChildDispositionRetentionAndCancellation(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	for i := range 8 {
		at := childTestTime.Add(time.Duration(i) * 3 * ChildRetention)
		child, request := dispositionFixture(t, s, fmt.Sprintf("parent-%d", i), at)
		d, err := s.RequestChildDisposition(t.Context(), request, at)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.KeepChildDispositionPlan(t.Context(), d, []byte("plan")); err != nil {
			t.Fatal(err)
		}
		d, err = s.ChildDisposition(t.Context(), child.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.PruneChildren(t.Context(), at.Add(10*ChildRetention), 100); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ChildDisposition(t.Context(), child.Identity); err != nil {
			t.Fatal("unresolved intent pruned", err)
		}
		if err := s.CompleteChildDisposition(t.Context(), d, at); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkChildParentSettled(t.Context(), child.Identity.ChildParent, at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PruneChildren(t.Context(), at.Add(ChildRetention), 100); err != nil {
			t.Fatal(err)
		}
		if n := childTableCount(t, s, "child_dispositions"); n != 0 {
			t.Fatal("disposition plans accumulated", n)
		}
	}
	child, request := dispositionFixture(t, s, "cancelled", childTestTime)
	if err := s.FenceChildParent(t.Context(), child.Identity.ChildParent, "operator", childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestChildDisposition(t.Context(), request, childTestTime); !errors.Is(err, ErrParentCancelled) {
		t.Fatal(err)
	}
}
