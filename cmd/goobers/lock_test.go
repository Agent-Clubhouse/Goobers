package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/daemonstate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/version"
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

func TestAcquireDaemonLockWritesIdentity(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "scheduler", "up.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	release, err := acquireDaemonLockWithTimeout(lockPath, root, instance.DefaultDaemonLivenessTimeout)
	if err != nil {
		t.Fatalf("acquireDaemonLockWithTimeout: %v", err)
	}
	defer release()

	f, err := os.Open(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := readDaemonIdentity(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("readDaemonIdentity: %v", err)
	}
	if identity == nil {
		t.Fatal("readDaemonIdentity returned nil")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	if identity.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", identity.PID, os.Getpid())
	}
	if identity.StartedAt.Before(before) || identity.StartedAt.After(time.Now().UTC()) {
		t.Errorf("startedAt = %s, want acquisition time", identity.StartedAt)
	}
	if identity.InstanceRoot != absoluteRoot {
		t.Errorf("instanceRoot = %q, want %q", identity.InstanceRoot, absoluteRoot)
	}
	if identity.Version != version.Get().String() {
		t.Errorf("version = %q, want %q", identity.Version, version.Get().String())
	}
	if identity.LivenessTimeoutMillis != instance.DefaultDaemonLivenessTimeout.Milliseconds() {
		t.Errorf("liveness timeout = %dms, want %dms", identity.LivenessTimeoutMillis, instance.DefaultDaemonLivenessTimeout.Milliseconds())
	}
}

func TestInspectDaemonLockReadsHeldIdentity(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "scheduler", "up.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}

	release, err := acquireDaemonLockWithTimeout(lockPath, root, instance.DefaultDaemonLivenessTimeout)
	if err != nil {
		t.Fatalf("acquireDaemonLockWithTimeout: %v", err)
	}
	defer release()

	running, identity, err := inspectDaemonLock(lockPath)
	if err != nil {
		t.Fatalf("inspectDaemonLock: %v", err)
	}
	if !running {
		t.Fatal("inspectDaemonLock reported held daemon lock as stopped")
	}
	if identity == nil {
		t.Fatal("inspectDaemonLock returned nil identity")
	}
	if identity.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", identity.PID, os.Getpid())
	}
	if identity.InstanceRoot != root {
		t.Errorf("instanceRoot = %q, want %q", identity.InstanceRoot, root)
	}
}

func TestInspectDaemonLivenessUsesPinnedTimeout(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "scheduler", "up.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	timeout := 5 * time.Minute
	release, err := acquireDaemonLockWithTimeout(lockPath, root, timeout)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	now := time.Now().UTC()
	if err := daemonstate.Refresh(lockPath, now.Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	running, _, liveness, err := inspectDaemonLiveness(lockPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if !running || !liveness.Healthy || liveness.Timeout != timeout {
		t.Fatalf("liveness = %+v, running = %t", liveness, running)
	}
}

func TestAcquireInstanceLockConflictIncludesHolderPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "up.lock")
	identity := daemonIdentity{
		PID:          os.Getpid(),
		StartedAt:    time.Date(2026, time.July, 16, 9, 0, 0, 0, time.UTC),
		InstanceRoot: "/tmp/goobers",
		Version:      "v0.3.0",
	}
	release, err := acquireInstanceLockWithIdentity(path, &identity)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	if _, err := acquireInstanceLock(path); err == nil {
		t.Fatal("expected second acquire to fail")
	} else if !strings.Contains(err.Error(), "already holds the lock") ||
		!strings.Contains(err.Error(), fmt.Sprintf("holder pid %d", os.Getpid())) {
		t.Fatalf("err = %v, want existing conflict message enriched with holder pid", err)
	}
}

func TestUpFailsFastOnSecondInstance(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)

	identity := daemonIdentity{
		PID:          os.Getpid(),
		StartedAt:    time.Date(2026, time.July, 16, 9, 0, 0, 0, time.UTC),
		InstanceRoot: root,
		Version:      "v0.3.0",
	}
	release, err := acquireInstanceLockWithIdentity(filepath.Join(l.SchedulerDir(), "up.lock"), &identity)
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
	if !strings.Contains(stderr, fmt.Sprintf("holder pid %d", os.Getpid())) {
		t.Fatalf("stderr = %q, want holder pid", stderr)
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
