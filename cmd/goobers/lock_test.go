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
