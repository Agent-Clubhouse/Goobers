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

func TestLockHelperProcess(t *testing.T) {
	if os.Getenv(lockHelperEnv) != "1" {
		return
	}
	held, err := Acquire(os.Getenv(lockHelperPathEnv))
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
