package triggerqueue

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCancellationObservationDoesNotReleaseUncertainStart(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c := pinnedControl(t, s, "attempted", "manual")
	if err := s.BeginDispatchAt(t.Context(), c.Record.ID, childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RequestStartCancellation(t.Context(), "web", c.Record.ID, controlCommand(), childTestTime); err != nil {
		t.Fatal(err)
	}
	got, err := s.CompleteStartCancellation(t.Context(), "web", c.Record.ID, "cancel-1", "confirmed", childTestTime.Add(time.Second))
	if err != nil || got.Record.State != Dispatching || got.CancellationOutcome != "confirmed" {
		t.Fatal(got, err)
	}
	replay, err := s.CompleteStartCancellation(t.Context(), "web", c.Record.ID, "cancel-1", "confirmed", childTestTime.Add(time.Hour))
	if err != nil || replay.CancellationObservedAt != got.CancellationObservedAt {
		t.Fatal(replay, err)
	}
	if _, err = s.CompleteStartCancellation(t.Context(), "web", c.Record.ID, "cancel-1", "already-terminal", childTestTime.Add(time.Hour)); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	acceptTest(t, s, "maintenance", childTestTime.Add(365*24*time.Hour))
	if retained, err := s.ByKey(t.Context(), "attempted"); err != nil || retained.State != Dispatching {
		t.Fatal("uncertainty released", retained, err)
	}
}
func TestPendingCancellationRetainsDispatchedPinsUntilObservation(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	c := pinnedControl(t, s, "dispatched", "manual")
	if err := s.BeginDispatchAt(t.Context(), c.Record.ID, childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(t.Context(), c.Record.ID, Dispatched, c.Scope.ReservedRunID, "", childTestTime); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RequestStartCancellation(t.Context(), "web", c.Record.ID, controlCommand(), childTestTime.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	acceptTest(t, s, "maintenance", childTestTime.Add(31*24*time.Hour))
	retained, err := s.RetainedPage(t.Context(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range retained {
		found = found || r.ID == c.Record.ID
	}
	if !found {
		t.Fatal("pending effect lost archive custody")
	}
	now := childTestTime.Add(32 * 24 * time.Hour)
	if _, err = s.CompleteStartCancellation(t.Context(), "web", c.Record.ID, "cancel-1", "already-terminal", now); err != nil {
		t.Fatal(err)
	}
	acceptTest(t, s, "inside-replay", now.Add(time.Hour))
	if _, err = s.ByKey(t.Context(), "dispatched"); err != nil {
		t.Fatal("lost cancellation replay", err)
	}
	acceptTest(t, s, "after-replay", now.Add(ReplayRetention+time.Hour))
	if _, err = s.ByKey(t.Context(), "dispatched"); err == nil {
		t.Fatal("confirmed history never pruned")
	}
}
