package triggerqueue

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitingReasonIsClosedAndCannotChangeDispatchCustody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	left, right := openTestStore(t, path), openTestStore(t, path)
	r, _, err := left.Accept(t.Context(), "waiting", "actor", []byte(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = left.SetWaitingReason(t.Context(), r.ID, WaitingCapacity); err != nil {
		t.Fatal(err)
	}
	got, err := right.Get(t.Context(), r.ID, "actor")
	if err != nil || got.State != Accepted || got.Reason != WaitingCapacity.Message() || !got.AcceptedAt.Equal(r.AcceptedAt) {
		t.Fatal(got, err)
	}
	if err = left.SetWaitingReason(t.Context(), r.ID, WaitingReason("provider secret-token")); !errors.Is(err, ErrTransition) {
		t.Fatal(err)
	}
	if err = right.BeginDispatch(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	if err = left.SetWaitingReason(t.Context(), r.ID, WaitingAccess); err != nil {
		t.Fatal(err)
	}
	got, err = right.Get(t.Context(), r.ID, "actor")
	if err != nil || got.State != Dispatching || got.Reason != "" {
		t.Fatal("observation changed attempted custody", got, err)
	}
}
