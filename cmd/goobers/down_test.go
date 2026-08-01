package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// TestDownDrivesLiveDaemonThroughGracefulShutdown is #2072's end-to-end
// acceptance: `goobers down` against a live `goobers up` daemon drives it
// through the identical drain-shutdown path SIGINT/SIGTERM already trigger
// — proven here by NEVER cancelling the daemon's own context; the only way
// runUpContext can return is via the stop-request file `goobers down` drops.
func TestDownDrivesLiveDaemonThroughGracefulShutdown(t *testing.T) {
	prevInterval := delegationSweepInterval
	delegationSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() { delegationSweepInterval = prevInterval })

	root := initDeterministicDemo(t)

	// Deliberately context.Background(), not WithCancel: this test asserts
	// the daemon stops WITHOUT its context ever being cancelled by the test,
	// so the only path to upDone closing is the stop-request file's own
	// supervisorStop -> stopDaemon() route (up.go).
	upStdout := &daemonStartedWriter{started: make(chan struct{})}
	var upStderr strings.Builder
	var upCode int
	upDone := make(chan struct{})
	go func() {
		upCode = runUpContext(context.Background(), []string{root}, upStdout, &upStderr)
		close(upDone)
	}()

	select {
	case <-upStdout.started:
	case <-upDone:
		t.Fatalf("runUpContext exited before startup: code = %d, stderr = %q", upCode, upStderr.String())
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for runUpContext to report daemon readiness")
	}

	var downStdout, downStderr strings.Builder
	code := runDown([]string{root}, &downStdout, &downStderr)
	if code != 0 {
		t.Fatalf("down: code = %d, stdout = %q, stderr = %q", code, downStdout.String(), downStderr.String())
	}
	if !strings.Contains(downStdout.String(), "shutdown requested") {
		t.Fatalf("down stdout = %q, want a shutdown-requested confirmation", downStdout.String())
	}

	select {
	case <-upDone:
	case <-time.After(10 * time.Second):
		t.Fatal("goobers down did not drive the live daemon to shut down")
	}
	if upCode != 0 {
		t.Fatalf("daemon exit code after `goobers down` = %d, stderr = %q", upCode, upStderr.String())
	}

	events, err := journal.ReadInstanceLog(instance.NewLayout(root).SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var sawCleanShutdown bool
	for _, ev := range events {
		if ev.Type == journal.EventDaemonCleanShutdown {
			sawCleanShutdown = true
		}
	}
	if !sawCleanShutdown {
		t.Fatalf("instance journal missing a clean-shutdown event: %+v", events)
	}
}

// TestDownFailsFastWithNoLiveDaemon covers the issue's second acceptance
// criterion: with no live daemon for the instance, `goobers down` must fail
// fast with a clear message rather than hanging or polling indefinitely.
func TestDownFailsFastWithNoLiveDaemon(t *testing.T) {
	root := initDemo(t)

	done := make(chan struct{})
	var code int
	var stdout, stderr strings.Builder
	go func() {
		code = runDown([]string{root}, &stdout, &stderr)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDown hung with no live daemon present")
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout = %q stderr = %q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "no live") {
		t.Fatalf("stderr = %q, want a clear no-live-daemon message", stderr.String())
	}
}

// TestDownRejectsExtraArg matches every other single-positional-arg command's
// usage-error convention.
func TestDownRejectsExtraArg(t *testing.T) {
	code, _, stderr := runArgs(t, "down", "a", "b")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "goobers down") {
		t.Fatalf("stderr = %q, want usage mentioning goobers down", stderr)
	}
}
