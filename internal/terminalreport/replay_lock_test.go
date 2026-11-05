package terminalreport

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

const (
	replayHelperEnv    = "GOOBERS_TERMINAL_REPORT_REPLAY_HELPER"
	replayHelperDirEnv = "GOOBERS_TERMINAL_REPORT_REPLAY_DIR"
)

func TestReplayLockExcludesProcessesAndReleasesAfterCrash(t *testing.T) {
	dir := t.TempDir()
	q := Queue{Dir: dir}
	e := Effect{Kind: WorkFailure, ItemID: "42", RunID: "run-1"}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Publish(cancelled, e, nil); err == nil {
		t.Fatal("cancelled publish unexpectedly delivered")
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestReplayLockHelperProcess$")
	cmd.Env = append(os.Environ(), replayHelperEnv+"=1", replayHelperDirEnv+"="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "replay-ready" {
		t.Fatalf("helper did not enter replay: %v", scanner.Err())
	}

	var calls atomic.Int32
	restarted := Queue{Dir: dir, Now: func() time.Time { return time.Now().Add(2 * time.Minute) }}
	if err := restarted.Reconcile(context.Background(), 1, func(context.Context, Effect) error {
		calls.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("second process delivered while replay owner held the outbox lock")
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := restarted.Reconcile(context.Background(), 1, func(context.Context, Effect) error {
		calls.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("deliveries after owner crash = %d, want exactly one", calls.Load())
	}
}

func TestReplayLockHelperProcess(t *testing.T) {
	if os.Getenv(replayHelperEnv) != "1" {
		return
	}
	q := Queue{Dir: os.Getenv(replayHelperDirEnv)}
	err := q.Reconcile(context.Background(), 1, func(context.Context, Effect) error {
		if _, err := fmt.Fprintln(os.Stdout, "replay-ready"); err != nil {
			return err
		}
		_, err := io.Copy(io.Discard, os.Stdin)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
