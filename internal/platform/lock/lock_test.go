package lock

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
	acquire := TryAcquire
	if os.Getenv("GOOBERS_LOCK_SHARED") == "1" {
		acquire = TryAcquireShared
	}
	held, err := acquire(os.Getenv(lockHelperPathEnv))
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

func startLockHelper(t *testing.T, path string, shared ...bool) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	cmd.Env = append(os.Environ(), lockHelperEnv+"=1", lockHelperPathEnv+"="+path)
	if len(shared) > 0 && shared[0] {
		cmd.Env = append(cmd.Env, "GOOBERS_LOCK_SHARED=1")
	}
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

func TestSharedReadersExcludeWriterUntilAllRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.lock")
	first, err := TryAcquireShared(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Release() })
	second, err := TryAcquireShared(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Release() })
	assertLockHeld(t, path)
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	assertLockHeld(t, path)
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	writer, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Release() }()
	if reader, err := TryAcquireShared(path); !errors.Is(err, ErrHeld) {
		if reader != nil {
			_ = reader.Release()
		}
		t.Fatalf("shared reader admitted over writer: %v", err)
	}
}

func TestSharedLeaseSurvivesOtherReaderCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.lock")
	child, _ := startLockHelper(t, path, true)
	reader, err := TryAcquireShared(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Release() })
	assertLockHeld(t, path)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	assertLockHeld(t, path)
	if err := reader.Release(); err != nil {
		t.Fatal(err)
	}
	assertLockAcquirable(t, path)
}
