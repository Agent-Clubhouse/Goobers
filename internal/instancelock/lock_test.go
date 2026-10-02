package instancelock

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

	release, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	if _, err := Acquire(path); err == nil {
		t.Fatalf("expected second acquire to fail while first holds the lock")
	} else if !strings.Contains(err.Error(), "already holds the lock") {
		t.Fatalf("err = %v", err)
	}
}

func TestAcquireInstanceLockReacquirableAfterRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "up.lock")

	release, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	release()

	release2, err := Acquire(path)
	if err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
	release2()
}

func TestInspectDaemonLockReadsHeldIdentity(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "scheduler", "up.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}

	identityAtAcquire := DaemonIdentity{
		PID:                   os.Getpid(),
		StartedAt:             time.Now().UTC(),
		InstanceRoot:          root,
		Version:               version.Get().String(),
		LivenessTimeoutMillis: instance.DefaultDaemonLivenessTimeout.Milliseconds(),
	}
	release, err := AcquireWithIdentity(lockPath, &identityAtAcquire)
	if err != nil {
		t.Fatalf("acquireDaemonLock: %v", err)
	}
	defer release()

	running, identity, err := InspectDaemonLock(lockPath)
	if err != nil {
		t.Fatalf("InspectDaemonLock: %v", err)
	}
	if !running {
		t.Fatal("InspectDaemonLock reported held daemon lock as stopped")
	}
	if identity == nil {
		t.Fatal("InspectDaemonLock returned nil identity")
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
	identityAtAcquire := DaemonIdentity{
		PID:                   os.Getpid(),
		StartedAt:             time.Now().UTC(),
		InstanceRoot:          root,
		Version:               version.Get().String(),
		LivenessTimeoutMillis: timeout.Milliseconds(),
	}
	release, err := AcquireWithIdentity(lockPath, &identityAtAcquire)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	now := time.Now().UTC()
	if err := daemonstate.Refresh(lockPath, now.Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	running, _, liveness, err := InspectDaemonLiveness(lockPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if !running || !liveness.Healthy || liveness.Timeout != timeout {
		t.Fatalf("liveness = %+v, running = %t", liveness, running)
	}
}

func TestAcquireInstanceLockConflictIncludesHolderPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "up.lock")
	identity := DaemonIdentity{
		PID:          os.Getpid(),
		StartedAt:    time.Date(2026, time.July, 16, 9, 0, 0, 0, time.UTC),
		InstanceRoot: "/tmp/goobers",
		Version:      "v0.3.0",
	}
	release, err := AcquireWithIdentity(path, &identity)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	if _, err := Acquire(path); err == nil {
		t.Fatal("expected second acquire to fail")
	} else if !strings.Contains(err.Error(), "already holds the lock") ||
		!strings.Contains(err.Error(), fmt.Sprintf("holder pid %d", os.Getpid())) {
		t.Fatalf("err = %v, want existing conflict message enriched with holder pid", err)
	}
}
