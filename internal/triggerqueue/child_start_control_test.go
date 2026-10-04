package triggerqueue

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func pinInitialChildControl(t *testing.T, s *Store, c ChildRecord) StartControl {
	t.Helper()
	record, err := s.ChildStart(t.Context(), c.Identity)
	if err != nil {
		t.Fatal(err)
	}
	scope := controlScope(record, "child")
	scope.Gaggle = c.Identity.Gaggle
	scope.ReservedRunID = c.RunID
	scope.Deadline = time.Time{}
	control, err := s.PinStartControl(t.Context(), record.ID, scope)
	if err != nil {
		t.Fatal(err)
	}
	return control
}

func TestInitialChildControlFencesLaunchAndRecoveryOnlyItsAcceptance(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "launch", true: "resume"}[resume], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "queue.db")
			s, other := openTestStore(t, path), openTestStore(t, path)
			child := acceptChildTest(t, s, childRequest("parent", "occurrence", "key"), childTestTime)
			control := pinInitialChildControl(t, s, child)
			sibling := acceptChildTest(t, s, childRequest("parent", "other-occurrence", "other"), childTestTime)
			for _, c := range []ChildRecord{child, sibling} {
				if err := s.BeginDispatch(t.Context(), c.AcceptanceID); err != nil {
					t.Fatal(err)
				}
			}
			barrier := s.WithChildLaunch
			if resume {
				barrier = s.WithChildResume
			}
			entered, proceed, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				done <- barrier(t.Context(), child.Identity, func() error { close(entered); <-proceed; return nil })
			}()
			<-entered
			cancelled := make(chan error, 1)
			go func() {
				_, _, err := other.RequestStartCancellation(t.Context(), control.Scope.Gaggle, control.Record.ID, controlCommand(), childTestTime.Add(time.Minute))
				cancelled <- err
			}()
			close(proceed)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := <-cancelled; err != nil {
				t.Fatal(err)
			}
			called := false
			if err := barrier(t.Context(), child.Identity, func() error { called = true; return nil }); !errors.Is(err, ErrTransition) || called {
				t.Fatal("cancelled original released", err, called)
			}
			if err := barrier(t.Context(), sibling.Identity, func() error { return nil }); err != nil {
				t.Fatal("sibling fenced", err)
			}
			retained, err := s.GetChild(t.Context(), child.Identity)
			if err != nil || retained.CancellationRequested || !retained.AcknowledgedAt.IsZero() {
				t.Fatal("family/parent custody altered", retained, err)
			}
		})
	}
}

func TestOriginalQueueCancellationDoesNotFenceHumanChildEpoch(t *testing.T) {
	s := openTestStore(t, filepath.Join(t.TempDir(), "queue.db"))
	child := acceptChildTest(t, s, childRequest("parent", "occurrence", "key"), childTestTime)
	control := pinInitialChildControl(t, s, child)
	if err := s.BeginDispatch(t.Context(), child.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	result := childResultValue("stopped initial", "")
	if err := s.KeepChildResult(t.Context(), child, result); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChildState(t.Context(), child.Identity, ChildStateUpdate{Expected: ChildQueued, State: ChildFailed, ResultRef: result.ReceiptDigest}, childTestTime); err != nil {
		t.Fatal(err)
	}
	child, err := s.GetChild(t.Context(), child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := s.BeginChildRestart(t.Context(), childRestartRequest(child, "human-next"), childTestTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.RequestStartCancellation(t.Context(), control.Scope.Gaggle, control.Record.ID, controlCommand(), childTestTime.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	called := false
	if err = s.WithChildExecutionResume(t.Context(), child.Identity, epoch.RunID, func() error { called = true; return nil }); err != nil || !called {
		t.Fatal("original control fenced new epoch", err, called)
	}
}
