//go:build integration && !windows

package executor

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/testdep"
)

// TestIntegrationShellExecutor_TimeoutGivesUpOnEscapedDescendant is the
// regression test for #119's WaitDelay gap: a grandchild that escapes the
// process group (via job control's own new-pgid-per-background-job behavior,
// the portable stand-in for setsid) survives the group kill and keeps the
// stdout pipe open, so cmd.Wait() would never return on its own. Run must still
// return within groupKillWaitDelay of the timeout rather than hanging for the
// escaped process's full lifetime.
func TestIntegrationShellExecutor_TimeoutGivesUpOnEscapedDescendant(t *testing.T) {
	testdep.Require(t, "bash", "sleep")

	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputTimeout: "100ms"}

	start := time.Now()
	// `set -m` gives the backgrounded sleep its own process group — the
	// portable equivalent of setsid — it outlives bash's own near-immediate
	// exit and is never reached by the group kill (bash's group, not its
	// own). 30s comfortably exceeds groupKillWaitDelay (5s), so the test can
	// only pass via the give-up bound, not by the escaped process happening
	// to exit on its own first.
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"bash", "-c", "set -m; sleep 30 & sleep 0.1"},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The escaped descendant ignores/outruns both signals, so Run gives up
	// after the timeout SIGQUIT grace AND the SIGKILL give-up bound:
	// ~timeout + timeoutDumpGrace + groupKillWaitDelay. Still bounded — it does
	// not hang for the escaped process's full 30s lifetime.
	if elapsed > timeoutDumpGrace+groupKillWaitDelay+3*time.Second {
		t.Fatalf("Run took %s, want under ~%s (timeout + timeoutDumpGrace + groupKillWaitDelay) — the give-up bound did not engage", elapsed, 100*time.Millisecond+timeoutDumpGrace+groupKillWaitDelay)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "timeout" {
		t.Fatalf("error = %+v, want timeout", result.Error)
	}
}

// TestIntegrationShellExecutor_DistinguishesCancelFromTimeout is #122's
// low-priority defense-in-depth item: runCtx.Done() fires both when its own
// timeout elapses and when the caller's ctx is externally canceled, and the
// two must not be conflated — a canceled ctx should never come back as the
// "timeout" error code. internal/runner's dispatch always uses
// context.WithoutCancel today, so this path is otherwise unreachable in
// production; the test drives it directly by canceling ctx itself rather than
// through the runner.
func TestIntegrationShellExecutor_DistinguishesCancelFromTimeout(t *testing.T) {
	testdep.Require(t, "sleep")

	shellExec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputTimeout: "10s"} // comfortably longer than the external cancel

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	result, err := shellExec.Run(ctx, env, apiv1.DeterministicRun{Command: []string{"sleep", "5"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "canceled" || result.Error.Retryable {
		t.Fatalf("error = %+v, want canceled, non-retryable", result.Error)
	}
}
