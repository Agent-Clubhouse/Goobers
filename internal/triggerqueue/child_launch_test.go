package triggerqueue

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestChildLaunchSerializesPublicationWithParentCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "starts.db")
	s := openTestStore(t, path)
	other := openTestStore(t, path)
	c := acceptChildTest(t, s, childRequest("parent", "occurrence", "key"), childTestTime)
	if err := s.BeginDispatch(t.Context(), c.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	entered, proceed := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- s.WithChildLaunch(t.Context(), c.Identity, func() error { close(entered); <-proceed; return nil })
	}()
	<-entered
	fenced := make(chan error, 1)
	go func() {
		fenced <- other.FenceChildParent(t.Context(), c.Identity.ChildParent, "operator", childTestTime)
	}()
	close(proceed)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := <-fenced; err != nil {
		t.Fatal(err)
	}
	called := false
	err := other.WithChildLaunch(t.Context(), c.Identity, func() error { called = true; return nil })
	if !errors.Is(err, ErrParentCancelled) || called {
		t.Fatalf("fenced publication: %v %v", err, called)
	}
	children, err := other.PendingChildCancellations(t.Context(), c.Identity.ChildParent, "", 100)
	if err != nil || len(children) != 1 {
		t.Fatalf("published child lost cancellation outbox: %v %v", children, err)
	}
}
