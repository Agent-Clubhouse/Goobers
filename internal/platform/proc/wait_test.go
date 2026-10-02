package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestWaitOrKillSuccess(t *testing.T) {
	wait := make(chan error, 1)
	wait <- nil

	outcome := WaitOrKill(context.Background(), nil, wait, WaitOptions{})
	if outcome != (WaitOutcome{}) {
		t.Fatalf("outcome = %+v, want success", outcome)
	}
}

func TestWaitOrKillChildExitError(t *testing.T) {
	want := errors.New("child exit")
	wait := make(chan error, 1)
	wait <- want

	outcome := WaitOrKill(context.Background(), nil, wait, WaitOptions{})
	if !errors.Is(outcome.Err, want) || outcome.TimedOut || outcome.Canceled || outcome.GaveUp {
		t.Fatalf("outcome = %+v, want child exit error", outcome)
	}
}

func TestWaitOrKillContextTimeout(t *testing.T) {
	tree, wait := startWaitHelper(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	beforeKill := false
	outcome := WaitOrKill(ctx, tree, wait, WaitOptions{
		KillWait: time.Second,
		BeforeKill: func(timedOut bool) (bool, error) {
			beforeKill = timedOut
			return false, nil
		},
	})
	if !outcome.TimedOut || outcome.Canceled || outcome.GaveUp {
		t.Fatalf("outcome = %+v, want timeout followed by child exit", outcome)
	}
	if !beforeKill {
		t.Fatal("BeforeKill did not receive timeout classification")
	}
	if outcome.Err == nil {
		t.Fatal("wait error = nil, want killed child exit error")
	}
}

func TestWaitOrKillContextCancellation(t *testing.T) {
	tree, wait := startWaitHelper(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	outcome := WaitOrKill(ctx, tree, wait, WaitOptions{KillWait: time.Second})
	if outcome.TimedOut || !outcome.Canceled || outcome.GaveUp {
		t.Fatalf("outcome = %+v, want cancellation followed by child exit", outcome)
	}
	if outcome.Err == nil {
		t.Fatal("wait error = nil, want killed child exit error")
	}
}

func TestWaitOrKillGivesUpAfterKillWait(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestWaitOrKillHelperProcess$")
	cmd.Env = append(os.Environ(), "GOOBERS_WAIT_HELPER=1")
	tree, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tree.Kill()
		_ = cmd.Wait()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	outcome := WaitOrKill(ctx, tree, make(chan error), WaitOptions{KillWait: 20 * time.Millisecond})
	if !outcome.Canceled || !outcome.GaveUp {
		t.Fatalf("outcome = %+v, want cancellation and give-up", outcome)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond || elapsed > time.Second {
		t.Fatalf("elapsed = %s, want bounded by kill-wait", elapsed)
	}
}

func TestWaitOrKillHelperProcess(t *testing.T) {
	if os.Getenv("GOOBERS_WAIT_HELPER") != "1" {
		return
	}
	select {}
}

func startWaitHelper(t *testing.T) (*Tree, <-chan error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWaitOrKillHelperProcess$")
	cmd.Env = append(os.Environ(), "GOOBERS_WAIT_HELPER=1")
	tree, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	t.Cleanup(func() { _ = tree.Kill() })
	return tree, wait
}
