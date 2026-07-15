package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

func TestAcquireInstanceLockExcludesConcurrentHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "up.lock")

	release, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	if _, err := acquireInstanceLock(path); err == nil {
		t.Fatalf("expected second acquire to fail while first holds the lock")
	} else if !strings.Contains(err.Error(), "already holds the lock") {
		t.Fatalf("err = %v", err)
	}
}

func TestAcquireInstanceLockReacquirableAfterRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "up.lock")

	release, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	release()

	release2, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
	release2()
}

func TestUpFailsFastOnSecondInstance(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)

	release, err := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	code, _, stderr := runArgs(t, "up", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "already holds the lock") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestRunLockConflictDelegatesRatherThanFailingImmediately is #343's
// supersession of #231: `goobers run` no longer fails the instant it finds
// the lock held (#231's stopgap fix only reworded that immediate error) — it
// now attempts delegation (rundelegate.go) instead. This test's lock holder
// is NOT a real daemon sweeping requests (nothing ever answers), so
// delegation must still fail eventually — but via a bounded, actionable
// timeout, not the old instant "stop it first" message. TestRunDelegatesToLiveDaemon
// (rundelegate_test.go) proves the success path against a real daemon.
func TestRunLockConflictDelegatesRatherThanFailingImmediately(t *testing.T) {
	prevTimeout := triggerDelegationTimeout
	triggerDelegationTimeout = 200 * time.Millisecond
	t.Cleanup(func() { triggerDelegationTimeout = prevTimeout })

	root := initDemo(t)
	l := instance.NewLayout(root)

	release, err := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	code, _, stderr := runArgs(t, "run", "whatever-workflow", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "timed out") {
		t.Fatalf("stderr = %q, want a delegation timeout (no real daemon is sweeping this lock holder's requests)", stderr)
	}
	if strings.Contains(stderr, "already holds the lock") {
		t.Fatalf("stderr = %q, should no longer report the old immediate lock-conflict error (#343 supersedes #231's stopgap)", stderr)
	}
}
