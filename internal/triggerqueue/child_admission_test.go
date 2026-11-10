package triggerqueue

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func heldChildRequest(key string) ChildAcceptance {
	req := childRequest("parent", "occurrence-"+key, key)
	req.AdmissionHold = &ChildAdmissionHold{Reason: "out-of-distribution", Evidence: []byte(`{"scored":true,"outOfDistribution":true}`)}
	return req
}

func TestChildAdmissionHoldFencesDispatchUntilApproved(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	c := acceptChildTest(t, s, heldChildRequest("approve"), childTestTime)
	review, err := s.ChildAdmission(t.Context(), c.Identity)
	if err != nil || review.State != ChildAdmissionPending || review.ChildID != c.ChildID || string(review.Evidence) != `{"scored":true,"outOfDistribution":true}` || !review.RequestedAt.Equal(childTestTime) || !review.DecidedAt.IsZero() {
		t.Fatalf("pending review = %+v, %v", review, err)
	}
	if err := s.BeginDispatch(t.Context(), c.AcceptanceID); !errors.Is(err, ErrTransition) {
		t.Fatalf("held dispatch = %v", err)
	}
	pending, err := s.PendingChildAdmissions(t.Context(), "", 10)
	if err != nil || len(pending) != 1 || pending[0].ChildID != c.ChildID {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	decision := ChildAdmissionDecision{ChildID: c.ChildID, Approve: true, Actor: "operator", Note: "reviewed"}
	decided, err := s.DecideChildAdmission(t.Context(), decision, childTestTime.Add(time.Minute))
	if err != nil || decided.State != ChildAdmissionApproved || decided.Actor != "operator" || decided.Note != "reviewed" || !decided.DecidedAt.Equal(childTestTime.Add(time.Minute)) {
		t.Fatalf("approve = %+v, %v", decided, err)
	}
	if again, err := s.DecideChildAdmission(t.Context(), decision, childTestTime.Add(time.Hour)); err != nil || !again.DecidedAt.Equal(decided.DecidedAt) || again.State != decided.State {
		t.Fatalf("idempotent approve = %+v, %v", again, err)
	}
	if _, err := s.DecideChildAdmission(t.Context(), ChildAdmissionDecision{ChildID: c.ChildID, Actor: "operator"}, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatalf("conflicting reject = %v", err)
	}
	if pending, err := s.PendingChildAdmissions(t.Context(), "", 10); err != nil || len(pending) != 0 {
		t.Fatalf("pending after approve = %+v, %v", pending, err)
	}
	if err := s.BeginDispatch(t.Context(), c.AcceptanceID); err != nil {
		t.Fatalf("approved dispatch = %v", err)
	}
}

func TestChildAdmissionRejectionSettlesStart(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	c := acceptChildTest(t, s, heldChildRequest("reject"), childTestTime)
	decided, err := s.DecideChildAdmission(t.Context(), ChildAdmissionDecision{ChildID: c.ChildID, Actor: "operator", Note: "unsafe"}, childTestTime.Add(time.Minute))
	if err != nil || decided.State != ChildAdmissionRejected {
		t.Fatalf("reject = %+v, %v", decided, err)
	}
	at, reason, err := s.ChildRejection(t.Context(), c.Identity)
	if err != nil || reason != ChildAdmissionRejectedReason || !at.Equal(childTestTime.Add(time.Minute)) {
		t.Fatalf("rejection = %v %q %v", at, reason, err)
	}
	if err := s.BeginDispatch(t.Context(), c.AcceptanceID); !errors.Is(err, ErrTransition) {
		t.Fatalf("rejected dispatch = %v", err)
	}
	if _, err := s.DecideChildAdmission(t.Context(), ChildAdmissionDecision{ChildID: c.ChildID, Approve: true, Actor: "operator"}, childTestTime); !errors.Is(err, ErrTransition) {
		t.Fatalf("approve after reject = %v", err)
	}
}

func TestChildAdmissionAbsentAndInvalid(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "starts.db"))
	c := acceptChildTest(t, s, childRequest("parent", "occurrence", "plain"), childTestTime)
	if _, err := s.ChildAdmission(t.Context(), c.Identity); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unheld admission = %v", err)
	}
	if _, err := s.DecideChildAdmission(t.Context(), ChildAdmissionDecision{ChildID: c.ChildID, Approve: true, Actor: "operator"}, childTestTime); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("decide unheld = %v", err)
	}
	if err := s.BeginDispatch(t.Context(), c.AcceptanceID); err != nil {
		t.Fatalf("unheld dispatch = %v", err)
	}
	bad := heldChildRequest("bad")
	bad.AdmissionHold.Evidence = make([]byte, MaxChildAdmissionEvidenceBytes+1)
	if _, _, err := s.AcceptChild(t.Context(), bad, childTestTime); err == nil {
		t.Fatal("oversized evidence accepted")
	}
}
