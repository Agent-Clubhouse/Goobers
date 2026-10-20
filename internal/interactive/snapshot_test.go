package interactive

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestIdleSnapshotRefusesActiveParentAndChild(t *testing.T) {
	e := newExecutor()
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	called := false
	capture := func() error { called = true; return nil }
	commandTest(t, s, "alice", session.ID, Command{Type: "message", Text: "work"})
	parent := receiveRun(t, e)
	requireCode(t, s.WithIdleSnapshot("alice", session.ID, capture), "idle_checkpoint_required")
	parent.result <- ExecutionResult{}
	waitStatus(t, s, "alice", session.ID, "idle")
	proposal := commandTest(t, s, "alice", session.ID, Command{Type: "child.request", Text: "review"})
	commandTest(t, s, "alice", session.ID, Command{Type: "child.review", ChildID: proposal.ChildID, Decision: "approve"})
	child := receiveRun(t, e)
	requireCode(t, s.WithIdleSnapshot("alice", session.ID, capture), "idle_checkpoint_required")
	if called {
		t.Fatal("snapshot ran while an execution was active")
	}
	child.result <- ExecutionResult{}
}

func TestIdleSnapshotRefusesWrongOwnerStoppedAndChangedPolicy(t *testing.T) {
	opts := options(t, newExecutor())
	s := openTest(t, opts)
	stopped := createTest(t, s, "alice")
	changed := createTest(t, s, "alice")
	capture := func() error { t.Error("rejected snapshot callback was invoked"); return nil }
	requireCode(t, s.WithIdleSnapshot("bob", stopped.ID, capture), "not_found")
	commandTest(t, s, "alice", stopped.ID, Command{Type: "stop"})
	requireCode(t, s.WithIdleSnapshot("alice", stopped.ID, capture), "invalid_state")
	next := clone(opts.Surfaces)
	next[0].ConfigRevision = "new"
	if err := s.ReloadSurfaces(next); err != nil {
		t.Fatal(err)
	}
	requireCode(t, s.WithIdleSnapshot("alice", changed.ID, capture), "surface_changed")
}

func TestIdleSnapshotBlocksNewTurnUntilCaptureFinishes(t *testing.T) {
	e := newExecutor()
	s := openTest(t, options(t, e))
	session := createTest(t, s, "alice")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	captureResult := make(chan error, 1)
	go func() {
		captureResult <- s.WithIdleSnapshot("alice", session.ID, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("capture never started")
	}
	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Fatal("snapshot did not hold admission mutex")
	}
	attempted, admitted := make(chan struct{}), make(chan error, 1)
	go func() {
		close(attempted)
		_, err := s.Command(context.Background(), "alice", session.ID, Command{CommandID: "after-copy", Type: "message", Text: "edit", ExpectedSequence: session.Sequence, ExpectedFence: session.Fence})
		admitted <- err
	}()
	<-attempted
	select {
	case err := <-admitted:
		t.Fatalf("turn admitted during file copy: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	unblock()
	if err := <-captureResult; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("admission did not resume after copy")
	}
	run := receiveRun(t, e)
	run.result <- ExecutionResult{}
}

func TestIdleSnapshotReturnsCaptureFailureWithoutMutatingSession(t *testing.T) {
	s := openTest(t, options(t, newExecutor()))
	session := createTest(t, s, "alice")
	copyErr := errors.New("bounded file copy failed")
	if err := s.WithIdleSnapshot("alice", session.ID, func() error { return copyErr }); !errors.Is(err, copyErr) {
		t.Fatal(err)
	}
	current, err := s.Session("alice", session.ID)
	if err != nil || current.Sequence != session.Sequence || current.Lease != nil || current.Status != "idle" {
		t.Fatalf("capture changed session: %+v %v", current, err)
	}
	if err := s.WithIdleSnapshot("alice", session.ID, func() error { return nil }); err != nil {
		t.Fatal("capture failure retained lock", err)
	}
}
