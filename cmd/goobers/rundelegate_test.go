package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// fakeDelegateStarter records every Start call and returns a canned result —
// a minimal localscheduler.Starter fake for rundelegate.go's unit tests,
// mirroring internal/localscheduler's own unexported fakeStarter (not
// reachable from this package).
type fakeDelegateStarter struct {
	result localscheduler.StartResult
	err    error
}

func (f *fakeDelegateStarter) Start(context.Context, localscheduler.StartRequest) (localscheduler.StartResult, error) {
	return f.result, f.err
}

func newTestDelegateScheduler(t *testing.T, entries []localscheduler.WorkflowEntry) (*localscheduler.Scheduler, string) {
	t.Helper()
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return localscheduler.New(entries, log), dir
}

// TestSweepDispatchesPendingRequest is #343's core protocol acceptance: a
// request file written by writeTriggerRequest gets picked up by
// sweepPendingTriggers, dispatched through the given Scheduler, and its
// response is readable via pollTriggerResponse — the same round trip
// runDelegatedTrigger/runUpContext drive in the real CLI.
func TestSweepDispatchesPendingRequest(t *testing.T) {
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   starter,
	}})

	requestID, err := writeTriggerRequest(schedulerDir, "implement")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}

	sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runID, err := pollTriggerResponse(ctx, schedulerDir, requestID, time.Second)
	if err != nil {
		t.Fatalf("pollTriggerResponse: %v", err)
	}
	if runID == "" {
		t.Fatal("expected a non-empty run id")
	}
}

// TestSweepConsumesRequestFileOnce proves a request file is removed once
// swept (dispatch's own "consume before dispatch" ordering, rundelegate.go's
// doc comment) — a second sweep pass must not re-dispatch the same request.
func TestSweepConsumesRequestFileOnce(t *testing.T) {
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   starter,
	}})

	if _, err := writeTriggerRequest(schedulerDir, "implement"); err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now)
	sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now) // must be a no-op

	entries, err := filepathGlobRequests(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected the request file to be consumed, found: %v", entries)
	}
}

func filepathGlobRequests(schedulerDir string) ([]string, error) {
	return filepath.Glob(filepath.Join(schedulerDir, pendingTriggersDir, "*"+requestSuffix))
}

// TestSweepUnknownWorkflowRespondsWithError proves a delegated request for a
// workflow that doesn't exist surfaces the same "unknown workflow" error
// Scheduler.Trigger itself returns — through the response file, not silently
// dropped.
func TestSweepUnknownWorkflowRespondsWithError(t *testing.T) {
	sched, schedulerDir := newTestDelegateScheduler(t, nil)

	requestID, err := writeTriggerRequest(schedulerDir, "no-such-workflow")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now)

	_, err = pollTriggerResponse(context.Background(), schedulerDir, requestID, time.Second)
	if err == nil {
		t.Fatal("expected an unknown-workflow error")
	}
	if !strings.Contains(err.Error(), "unknown workflow") {
		t.Fatalf("err = %v, want it to mention unknown workflow", err)
	}
}

// TestPollTriggerResponseTimesOutWithNoSweeper proves pollTriggerResponse
// fails closed (bounded timeout, actionable error) rather than hanging
// forever when nothing ever sweeps the request — e.g. the daemon exited
// between this process observing up.lock held and writing its request.
func TestPollTriggerResponseTimesOutWithNoSweeper(t *testing.T) {
	schedulerDir := t.TempDir()
	requestID, err := writeTriggerRequest(schedulerDir, "implement")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}

	start := time.Now()
	_, err = pollTriggerResponse(context.Background(), schedulerDir, requestID, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout message", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %s, want it bounded close to the 200ms timeout", elapsed)
	}
}

// daemonStartedWriter turns runUpContext's existing startup message into a
// readiness signal. That message is emitted only after the daemon owns the
// instance lock and starts its delegation sweeper.
type daemonStartedWriter struct {
	started chan struct{}
	once    sync.Once
}

func (w *daemonStartedWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("daemon started")) {
		w.once.Do(func() { close(w.started) })
	}
	return len(p), nil
}

// TestRunDelegatesToLiveDaemon is #343's end-to-end CLI acceptance: with a
// real `goobers up` daemon holding the instance lock, `goobers run
// <workflow>` no longer fails — it delegates through the daemon and the
// dispatched run shows up identically to a daemon-native dispatch, per the
// issue's own literal test plan.
func TestRunDelegatesToLiveDaemon(t *testing.T) {
	prevInterval := delegationSweepInterval
	delegationSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() { delegationSweepInterval = prevInterval })

	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upStdout := &daemonStartedWriter{started: make(chan struct{})}
	var upStderr bytes.Buffer
	var upCode int
	upDone := make(chan struct{})
	go func() {
		upCode = runUpContext(ctx, []string{root}, upStdout, &upStderr)
		close(upDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-upDone:
		case <-time.After(10 * time.Second):
			t.Error("runUpContext did not shut down during cleanup")
		}
	})

	select {
	case <-upStdout.started:
	case <-upDone:
		t.Fatalf("runUpContext exited before startup: code = %d, stderr = %q", upCode, upStderr.String())
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for runUpContext to report daemon readiness")
	}

	code, stdout, stderr := runArgs(t, "run", "default-implement", root)
	if code != 0 {
		t.Fatalf("run: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "dispatched via live daemon") {
		t.Fatalf("stdout = %q, want a mention of live-daemon delegation", stdout)
	}
	if !strings.Contains(stdout, "phase=completed") {
		t.Fatalf("stdout = %q, want the delegated run to reach a terminal phase", stdout)
	}

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var sawFired, sawStarted bool
	for _, ev := range events {
		if ev.Workflow != "default-implement" {
			continue
		}
		if ev.Type == journal.EventTriggerFired && ev.Reason == "manual" {
			sawFired = true
		}
		if ev.Type == journal.EventRunStarted {
			sawStarted = true
		}
	}
	if !sawFired || !sawStarted {
		t.Fatalf("expected the delegated run visible in the daemon's own instance journal: %+v", events)
	}
}
