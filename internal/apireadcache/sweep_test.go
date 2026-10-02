package apireadcache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestStartLockSweepStopsOnCancellation(t *testing.T) {
	dir := t.TempDir()
	path := staleSweepLock(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := StartLockSweep(ctx, dir, time.Hour)
	t.Cleanup(func() { cancel(); joinLockSweep(t, done) })
	select {
	case <-done:
		t.Fatal("sweep stopped before cancellation")
	default:
	}
	cancel()
	joinLockSweep(t, done)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock changed before the first scheduled sweep: %v", err)
	}
}

func TestStartLockSweepRemovesStaleLockOnTick(t *testing.T) {
	dir := t.TempDir()
	path := staleSweepLock(t, dir)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watcher.Close() }()
	if err := watcher.Add(dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := StartLockSweep(ctx, dir, 10*time.Millisecond)
	t.Cleanup(func() { cancel(); joinLockSweep(t, done) })
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-watcher.Events:
			if event.Name != path || !event.Has(fsnotify.Remove) {
				continue
			}
			cancel()
			joinLockSweep(t, done)
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("stale lock survived sweep: %v", err)
			}
			return
		case err := <-watcher.Errors:
			t.Fatalf("watch sweep removal: %v", err)
		case <-deadline.C:
			t.Fatal("periodic sweep did not remove the stale lock")
		}
	}
}

func staleSweepLock(t *testing.T, dir string) string {
	t.Helper()
	path := apiReadListLockPath(dir, "periodic-sweep-key")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-apiReadCacheStaleLockAge - time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	return path
}

func joinLockSweep(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("lock sweep did not stop after cancellation")
	}
}
