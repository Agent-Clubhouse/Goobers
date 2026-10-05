package triggerqueue

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestChildResumeBarrierSerializesRunningReceiptWithCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s, other := openTestStore(t, path), openTestStore(t, path)
	c := acceptChildTest(t, s, childRequest("parent", "occurrence", "key"), childTestTime)
	if err := s.BeginDispatch(t.Context(), c.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(t.Context(), c.AcceptanceID, Dispatched, c.RunID, "", childTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildState(t.Context(), c.Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildRunning}, childTestTime); err != nil {
		t.Fatal(err)
	}
	entered, proceed, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.WithChildResume(t.Context(), c.Identity, func() error { close(entered); <-proceed; return nil })
	}()
	<-entered
	fenced := make(chan error, 1)
	go func() {
		fenced <- other.FenceChildParent(t.Context(), c.Identity.ChildParent, "operator", childTestTime)
	}()
	close(proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-fenced; err != nil {
		t.Fatal(err)
	}
	called := false
	if err := other.WithChildResume(t.Context(), c.Identity, func() error { called = true; return nil }); !errors.Is(err, ErrParentCancelled) || called {
		t.Fatal(err, called)
	}
}
