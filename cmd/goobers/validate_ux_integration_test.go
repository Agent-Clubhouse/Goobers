//go:build integration && !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/testdep"
)

func TestIntegrationGitRepositoryReachableTimeoutKillsDescendantHoldingOutputPipe(t *testing.T) {
	testdep.Require(t, "bash", "sleep")

	pidFile := installHangingGit(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		result <- gitRepositoryReachable(ctx, instance.RepoRef{
			Provider: "github",
			Owner:    "example",
			Name:     "repository",
		}, "test-token")
	}()
	waitForFile(t, pidFile)

	start := time.Now()
	cancel()
	err := <-result
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("gitRepositoryReachable error = %v, want canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("gitRepositoryReachable took %s; descendant inherited its output pipe after timeout", elapsed)
	}
}

func TestIntegrationGitRepositoryReachableTimeoutBoundedWhenEscapedDescendantHoldsOutputPipe(t *testing.T) {
	testdep.Require(t, "bash", "sleep")

	pidFile := installHangingGit(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		result <- gitRepositoryReachable(ctx, instance.RepoRef{
			Provider: "github",
			Owner:    "example",
			Name:     "repository",
		}, "test-token")
	}()
	waitForFile(t, pidFile)

	start := time.Now()
	cancel()
	err := <-result
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("gitRepositoryReachable error = %v, want canceled", err)
	}
	if elapsed > repositoryKillWaitDelay+2*time.Second {
		t.Fatalf("gitRepositoryReachable took %s; bounded wait did not engage after %s", elapsed, repositoryKillWaitDelay)
	}
}

// waitForFileTimeout bounds how long waitForFile waits for the hanging-git
// fixture's descendant to fork/exec and write its pid file. This is a SETUP
// precondition — the behavior under test is the subsequent cancel-and-kill,
// bounded separately by the `elapsed` assertions — so the ceiling is
// deliberately generous: waitForFile returns the instant the file appears
// (typically well under 100ms), so a large ceiling costs the happy path
// nothing, while a tight one (the previous 2s) intermittently timed out purely
// because spawning an external bash process gets starved under the full
// `-race` suite's CPU contention (#1145). Not a retry band-aid — it fixes an
// assumption (2s is always enough to schedule a subprocess) that is false under
// load.
const waitForFileTimeout = 30 * time.Second

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(waitForFileTimeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("wait for %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not created within %s (fixture descendant never spawned)", path, waitForFileTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func installHangingGit(t *testing.T, escapeProcessGroup bool) string {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	script := "#!" + exec.Command("bash").Path + "\n"
	if escapeProcessGroup {
		script += "set -m\n"
	}
	script += "sleep 10 &\necho $! > \"$GOOBERS_TEST_CHILD_PID_FILE\"\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_TEST_CHILD_PID_FILE", pidFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	})
	return pidFile
}
