package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

// TestApplyRoundTripsThroughLiveDaemonSweep is #459's end-to-end happy path
// for the request/response file protocol and the runApply CLI formatting: a
// live daemon (simulated by holding up.lock, exactly as dashboard_test.go's
// attach tests do) sweeps a pending request with a reconciler that reports a
// successful reconcile, and runApply reports the applied transition.
func TestApplyRoundTripsThroughLiveDaemonSweep(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	release, err := acquireDaemonLock(filepath.Join(l.SchedulerDir(), "up.lock"), root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	reconcile := func(ctx context.Context, now time.Time) applyResponse {
		return applyResponse{Applied: true, OldDigest: "sha256:old", NewDigest: "sha256:new", Revision: "abc123"}
	}
	stop := runApplySweepLoop(t, l.SchedulerDir(), reconcile)
	defer stop()

	var stdout, stderr bytes.Buffer
	code := runApply([]string{root}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0, stderr = %q", code, stderr.String())
	}
	if got := stdout.String(); got != "applied sha256:old -> sha256:new (revision abc123)\n" {
		t.Fatalf("stdout = %q, want applied transition with revision", got)
	}
}

// TestApplyReportsRejectedConfig: a pulled/edited config that fails
// validation must leave the daemon on its last-known-good definitions and
// report the rejection to the operator, exit code 1 — never silently
// succeed or swap to invalid definitions.
func TestApplyReportsRejectedConfig(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	release, err := acquireDaemonLock(filepath.Join(l.SchedulerDir(), "up.lock"), root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	reconcile := func(ctx context.Context, now time.Time) applyResponse {
		return applyResponse{Rejected: "config directory invalid: workflow default-implement: unknown task"}
	}
	stop := runApplySweepLoop(t, l.SchedulerDir(), reconcile)
	defer stop()

	var stdout, stderr bytes.Buffer
	code := runApply([]string{root}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1, stdout = %q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "keeping last-known-good definitions") ||
		!strings.Contains(stderr.String(), "unknown task") {
		t.Fatalf("stderr = %q, want rejection message with cause", stderr.String())
	}
}

// TestApplyReportsOperationalError distinguishes a config-validation
// rejection from an operational failure (e.g. the git fetch itself failing)
// that isn't a judgment about the pulled config's validity.
func TestApplyReportsOperationalError(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	release, err := acquireDaemonLock(filepath.Join(l.SchedulerDir(), "up.lock"), root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	reconcile := func(ctx context.Context, now time.Time) applyResponse {
		return applyResponse{Error: "sync workflow source: fetch main: connection refused"}
	}
	stop := runApplySweepLoop(t, l.SchedulerDir(), reconcile)
	defer stop()

	var stdout, stderr bytes.Buffer
	code := runApply([]string{root}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1, stdout = %q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "connection refused") {
		t.Fatalf("stderr = %q, want the sync error", stderr.String())
	}
}

// TestApplyReportsAlreadyCurrent: no change to reconcile is success, not an
// error — the operator asked to reconcile now and the answer is "there was
// nothing to do."
func TestApplyReportsAlreadyCurrent(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	release, err := acquireDaemonLock(filepath.Join(l.SchedulerDir(), "up.lock"), root, instance.DefaultDaemonLivenessTimeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	reconcile := func(ctx context.Context, now time.Time) applyResponse {
		return applyResponse{OldDigest: "sha256:same"}
	}
	stop := runApplySweepLoop(t, l.SchedulerDir(), reconcile)
	defer stop()

	var stdout, stderr bytes.Buffer
	code := runApply([]string{root}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0, stderr = %q", code, stderr.String())
	}
	if got := stdout.String(); got != "already current (digest sha256:same); nothing to apply\n" {
		t.Fatalf("stdout = %q, want already-current message", got)
	}
}

// TestApplyFailsFastWithNoLiveDaemon: with no daemon holding up.lock there is
// nothing to delegate the reconcile to, so apply refuses immediately rather
// than writing a request no one will ever sweep.
func TestApplyFailsFastWithNoLiveDaemon(t *testing.T) {
	root := initDeterministicDemo(t)

	code, stdout, stderr := runArgs(t, "apply", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1, stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "no live `goobers up` daemon found") {
		t.Fatalf("stderr = %q, want no-live-daemon message", stderr)
	}
}

// TestApplyRejectsExtraArg is a usage error, not a daemon-communication one.
func TestApplyRejectsExtraArg(t *testing.T) {
	code, stdout, _ := runArgs(t, "apply", "a", "b")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
}

// runApplySweepLoop runs sweepPendingApplyRequests on a short poll interval
// for the test's duration, simulating the daemon's own periodic delegation
// sweep (up.go) without spinning up a full `goobers up` process. The
// returned stop func blocks until the loop goroutine has exited, so it is
// safe to keep asserting on t after calling it.
func runApplySweepLoop(t *testing.T, schedulerDir string, reconcile applyReconciler) (stop func()) {
	t.Helper()
	done := make(chan struct{})
	quit := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				if err := sweepPendingApplyRequests(context.Background(), schedulerDir, reconcile, time.Now); err != nil {
					t.Errorf("sweepPendingApplyRequests: %v", err)
				}
			}
		}
	}()
	return func() {
		close(quit)
		<-done
	}
}
