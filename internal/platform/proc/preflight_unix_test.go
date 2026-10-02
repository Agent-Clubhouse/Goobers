//go:build unix

package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCleanupProbeOwnedDescendants(t *testing.T) {
	got := ProbeCleanup(context.Background())
	if got.Code != "owned_cleanup_observed" || got.Outcome != "passed" {
		t.Fatalf("fixture did not prove no surviving descendants: %+v", got)
	}
}

func TestCleanupProbeDoesNotTouchUnrelatedProcess(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	tree, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Kill(); _ = cmd.Wait() }()
	if got := ProbeCleanup(context.Background()); got.Outcome != "passed" {
		t.Fatal(got)
	}
	if !Alive(cmd.Process.Pid) {
		t.Fatal("probe killed an unrelated owned test process")
	}
}

func TestCleanupProbeDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if got := ProbeCleanup(ctx); got.Code != "cleanup_probe_canceled" {
		t.Fatal(got)
	}
}

func TestCleanupFixtureWaitHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command("/bin/sleep", "30")
	tree, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Kill(); _ = cmd.Wait() }()
	cancel()
	if awaitFixtureStopped(ctx, cmd.Process.Pid) {
		t.Fatal("live fixture was reported stopped")
	}
}

func TestCleanupFixtureCancellationStopsDescendant(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint("deadline=", deadline), func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			}
			defer cancel()
			// Do not emit the readiness line: the probe must clean up even
			// when cancellation/deadline arrives during fixture startup.
			cmd := exec.Command("/bin/sh", "-c", `/bin/sleep 30 & printf '%s\n' "$!" > "$1"; wait`, "fixture", pidFile)
			cmd.Env = []string{}
			done := make(chan CleanupProbe, 1)
			go func() { done <- runCleanupFixture(ctx, cmd) }()
			child := waitCleanupFixturePID(t, pidFile)
			if !deadline {
				cancel()
			}
			select {
			case got := <-done:
				if got.Code != "cleanup_probe_canceled" {
					t.Fatal(got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("canceled fixture did not return")
			}
			if !awaitFixtureStopped(context.Background(), cmd.Process.Pid, child) {
				t.Fatal("canceled fixture left a surviving process")
			}
		})
	}
}

func waitCleanupFixturePID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fixture did not start descendant")
	return 0
}
