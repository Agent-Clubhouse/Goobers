package main

import (
	"path/filepath"
	"strings"
	"testing"

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

// TestRunLockConflictDoesNotImplyNonexistentDelegation is #231's regression:
// the lock-conflict error `goobers run` surfaces while a `goobers up` daemon
// holds the instance lock must not imply a workflow-trigger delegation
// capability that doesn't actually exist yet (cmd/goobers/run.go's own doc
// comment: "no IPC/API surface exists yet for a short-lived `run` process to
// delegate to a long-running `up` process") — the only real option today is
// to stop the daemon first, so the message should say that plainly instead of
// dangling a nonexistent "trigger workflows through it" option.
func TestRunLockConflictDoesNotImplyNonexistentDelegation(t *testing.T) {
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
	if !strings.Contains(stderr, "already holds the lock") {
		t.Fatalf("stderr = %q, want it to mention the lock conflict", stderr)
	}
	if !strings.Contains(stderr, "stop it first") {
		t.Fatalf("stderr = %q, want actionable advice to stop the daemon", stderr)
	}
	if strings.Contains(stderr, "trigger workflows through it") {
		t.Fatalf("stderr = %q, still implies a delegation capability that does not exist (#231)", stderr)
	}
}
