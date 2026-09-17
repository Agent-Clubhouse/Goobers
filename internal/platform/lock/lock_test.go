package lock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	lockHelperEnv     = "GOOBERS_LOCK_HELPER"
	lockHelperPathEnv = "GOOBERS_LOCK_HELPER_PATH"
)

func TestCrossProcessContentionAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	cmd, stdin := startLockHelper(t, path)

	assertLockHeld(t, path)
	if err := stdin.Close(); err != nil {
		t.Fatalf("signal helper release: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for helper release: %v", err)
	}
	assertLockAcquirable(t, path)
}

func TestCrossProcessCrashReleasesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	cmd, _ := startLockHelper(t, path)

	assertLockHeld(t, path)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = cmd.Wait()
	assertLockAcquirable(t, path)
}

func TestTryAcquireExistingDoesNotCreateMissingLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.lock")
	held, err := TryAcquireExisting(path)
	if held != nil {
		_ = held.Release()
		t.Fatal("TryAcquireExisting returned a handle for a missing lock")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TryAcquireExisting error = %v, want os.ErrNotExist", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("TryAcquireExisting created missing lock: %v", statErr)
	}
}

func TestLockHelperProcess(t *testing.T) {
	if os.Getenv(lockHelperEnv) != "1" {
		return
	}
	held, err := TryAcquire(os.Getenv(lockHelperPathEnv))
	if err != nil {
		t.Fatalf("helper acquire: %v", err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "lock-ready"); err != nil {
		t.Fatalf("helper ready signal: %v", err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatalf("helper wait: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("helper release: %v", err)
	}
}

func startLockHelper(t *testing.T, path string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	cmd.Env = append(os.Environ(), lockHelperEnv+"=1", lockHelperPathEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("helper stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if scanner.Text() == "lock-ready" {
			return cmd, stdin
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read helper ready signal: %v", err)
	}
	_ = cmd.Wait()
	t.Fatal("helper exited before acquiring lock")
	return nil, nil
}

func assertLockHeld(t *testing.T, path string) {
	t.Helper()
	held, err := TryAcquire(path)
	if err == nil {
		_ = held.Release()
		t.Fatal("TryAcquire succeeded while helper held the lock")
	}
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("TryAcquire error = %v, want ErrHeld", err)
	}
}

func assertLockAcquirable(t *testing.T, path string) {
	t.Helper()
	held, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire after release: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("release reacquired lock: %v", err)
	}
}

// A brief holder must NOT fail the caller. This is the case that cost the
// goobernetes cloud instance a day: TryAcquire lost one collision, a worktree
// finalize aborted, a stalled run could never be terminalized, and a
// maxConcurrentRuns:1 lane wedged permanently (#5272).
func TestAcquireWithinWaitsOutABriefHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contended.lock")
	held, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = held.Release()
		close(released)
	}()

	start := time.Now()
	handle, err := AcquireWithin(context.Background(), path, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireWithin gave up on a holder that released after 60ms: %v", err)
	}
	t.Cleanup(func() { _ = handle.Release() })
	<-released
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("acquired in %s — before the holder released, so the lock was not exclusive", elapsed)
	}
}

// The wait is bounded, not infinite: a holder that never releases must still
// surface as contention rather than hanging the caller forever.
func TestAcquireWithinStillRefusesAWedgedHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wedged.lock")
	held, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })

	start := time.Now()
	if _, err := AcquireWithin(context.Background(), path, 120*time.Millisecond); !errors.Is(err, ErrHeld) {
		t.Fatalf("error = %v, want ErrHeld so the caller can report contention", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("returned after %s — did not wait out its own bound", elapsed)
	}
}

// Cancellation beats the bound, so a shutting-down daemon is not held up.
func TestAcquireWithinHonorsContextCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "canceled.lock")
	held, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	start := time.Now()
	if _, err := AcquireWithin(ctx, path, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("ignored cancellation for %s", elapsed)
	}
}
