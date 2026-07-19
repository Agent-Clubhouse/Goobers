package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
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

	mu    sync.Mutex
	calls int
}

func (f *fakeDelegateStarter) Start(context.Context, localscheduler.StartRequest) (localscheduler.StartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.result, f.err
}

func (f *fakeDelegateStarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
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
// testResponseWait bounds how long a test waits for a trigger response that
// a sweep has ALREADY been asked to produce. It is a failsafe against a
// genuinely stuck writer, never a timing assertion.
//
// pollTriggerResponse checks the response file before it ever sleeps and
// returns the instant it lands, so a generous bound costs nothing on the
// happy path — while a tight one turns ordinary CI load into a red build.
// TestSweepFailsFastOnNonTransientRefusal flaked exactly that way: a
// one-second bound reported "timed out waiting for the live daemon" instead
// of the run-conditions rejection it was actually asserting.
//
// The single test that is genuinely ABOUT the timeout
// (TestPollTriggerResponseTimesOutWithNoSweeper) keeps its own short,
// deliberate bound and must not use this.
const testResponseWait = 30 * time.Second

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

	if err := sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runID, err := pollTriggerResponse(ctx, schedulerDir, requestID, testResponseWait)
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
	if err := sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now); err != nil {
		t.Fatalf("first sweepPendingTriggers: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now); err != nil {
		t.Fatalf("second sweepPendingTriggers: %v", err)
	}

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

func writeTriggerRequestFixture(t *testing.T, schedulerDir, requestID string, req triggerRequest) {
	t.Helper()
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reqDir, requestID+requestSuffix), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSweepRefusesStaleRequestAndJournalsNote(t *testing.T) {
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   starter,
	}})
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	const requestID = "stale"
	writeTriggerRequestFixture(t, schedulerDir, requestID, triggerRequest{
		Workflow:  "implement",
		CreatedAt: now.Add(-triggerDelegationTimeout - time.Second),
	})

	if err := sweepPendingTriggers(context.Background(), schedulerDir, sched, func() time.Time { return now }); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}

	if starter.count() != 0 {
		t.Fatalf("starter calls = %d, want 0", starter.count())
	}
	requests, err := filepathGlobRequests(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatalf("stale request was not consumed: %v", requests)
	}
	_, err = pollTriggerResponse(context.Background(), schedulerDir, requestID, testResponseWait)
	if err == nil || !strings.Contains(err.Error(), "stale trigger request") || !strings.Contains(err.Error(), "refusing to dispatch") {
		t.Fatalf("pollTriggerResponse error = %v, want a stale-request refusal", err)
	}

	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped && ev.Workflow == "implement" && strings.Contains(ev.Reason, "stale trigger request") {
			return
		}
	}
	t.Fatalf("stale-request refusal was not journaled: %+v", events)
}

func TestSweepCollectsExpiredOrphanResponse(t *testing.T) {
	sched, schedulerDir := newTestDelegateScheduler(t, nil)
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(reqDir, "old"+responseSuffix)
	freshPath := filepath.Join(reqDir, "fresh"+responseSuffix)
	for _, path := range []string{oldPath, freshPath} {
		if err := os.WriteFile(path, []byte(`{"runId":"orphan"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	old := now.Add(-triggerDelegationTimeout - time.Second)
	fresh := now.Add(-triggerDelegationTimeout + time.Second)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(freshPath, fresh, fresh); err != nil {
		t.Fatal(err)
	}

	if err := sweepPendingTriggers(context.Background(), schedulerDir, sched, func() time.Time { return now }); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}

	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired orphan response stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("fresh response was removed: %v", err)
	}
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
	if err := sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}

	_, err = pollTriggerResponse(context.Background(), schedulerDir, requestID, testResponseWait)
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

func TestRunUpSweepsStaleDelegationAtStartup(t *testing.T) {
	prevInterval := delegationSweepInterval
	delegationSweepInterval = time.Hour
	t.Cleanup(func() { delegationSweepInterval = prevInterval })

	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const requestID = "predates-daemon"
	writeTriggerRequestFixture(t, l.SchedulerDir(), requestID, triggerRequest{
		Workflow:  "no-such-workflow",
		CreatedAt: time.Now().Add(-triggerDelegationTimeout - time.Minute),
	})

	reqDir := filepath.Join(l.SchedulerDir(), pendingTriggersDir)
	orphanPath := filepath.Join(reqDir, "startup-orphan"+responseSuffix)
	if err := os.WriteFile(orphanPath, []byte(`{"runId":"orphan"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-triggerDelegationTimeout - time.Minute)
	if err := os.Chtimes(orphanPath, old, old); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
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

	_, err := pollTriggerResponse(context.Background(), l.SchedulerDir(), requestID, testResponseWait)
	if err == nil || !strings.Contains(err.Error(), "stale trigger request") {
		t.Fatalf("startup refusal error = %v, want stale trigger request", err)
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup orphan response stat error = %v, want not exist", err)
	}
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped && strings.Contains(ev.Reason, "stale trigger request") {
			return
		}
	}
	t.Fatalf("startup stale-request refusal was not journaled: %+v", events)
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

func TestUpJournalsDelegationSweepError(t *testing.T) {
	prevInterval := delegationSweepInterval
	delegationSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() { delegationSweepInterval = prevInterval })

	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	if err := os.WriteFile(filepath.Join(l.SchedulerDir(), pendingTriggersDir), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := &daemonStartedWriter{started: make(chan struct{})}
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, []string{root}, started, &stderr) }()

	select {
	case <-started.started:
	case code := <-done:
		t.Fatalf("runUpContext exited before startup: code = %d, stderr = %q", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	event := waitForInstanceError(t, l.SchedulerDir(), "trigger_sweep_failed")
	if !strings.Contains(event.Error.Message, "read pending triggers") {
		t.Fatalf("trigger sweep error = %q, want pending-trigger read detail", event.Error.Message)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runUpContext did not return after cancellation")
	}
	if strings.Contains(stderr.String(), "trigger_sweep_failed") {
		t.Fatalf("trigger sweep error leaked to stderr: %q", stderr.String())
	}
}

func TestRunNoWaitDelegatesToLiveDaemon(t *testing.T) {
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

	code, stdout, stderr := runArgs(t, "run", "default-implement", "--no-wait", root)
	if code != 0 {
		t.Fatalf("run --no-wait: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	runID := runIDFromRunStdout(t, stdout)
	if !strings.Contains(stdout, "dispatched via live daemon") {
		t.Fatalf("stdout = %q, want a mention of live-daemon delegation", stdout)
	}
	if !strings.Contains(stdout, "inspect with: goobers trace "+runID+" "+root) {
		t.Fatalf("stdout = %q, want the trace hint", stdout)
	}
	if strings.Contains(stdout, "finished:") {
		t.Fatalf("stdout = %q, --no-wait must not report a terminal phase", stdout)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	phase, err := waitForRunTerminal(waitCtx, l.RunsDir(), runID)
	if err != nil {
		t.Fatalf("wait for delegated run: %v", err)
	}
	if phase != journal.PhaseCompleted {
		t.Fatalf("phase = %s, want completed", phase)
	}

	code, statusOut, stderr := runArgs(t, "status", root)
	if code != 0 || !strings.Contains(statusOut, runID) {
		t.Fatalf("status: code = %d, stdout = %q, stderr = %q", code, statusOut, stderr)
	}
	code, traceOut, stderr := runArgs(t, "trace", runID, root)
	if code != 0 || !strings.Contains(traceOut, "run.finished status=completed") {
		t.Fatalf("trace: code = %d, stdout = %q, stderr = %q", code, traceOut, stderr)
	}
}

// TestPollTriggerResponseToleratesTornWrite pins the fix for the #745 flake:
// the response writer uses a non-atomic os.WriteFile, so pollTriggerResponse can
// read the file in the window between its O_TRUNC and the content landing —
// empty or partial bytes that don't parse. It must treat that as "not ready
// yet" and re-poll (without consuming the file), not hard-fail the delegation.
// Pre-fix, a torn read returned an error → `goobers run` exited 1 with empty
// stdout, which for terminal phases that also exit 1 slipped past the exit-code
// check and failed the phase assertion intermittently under CI load.
func TestPollTriggerResponseToleratesTornWrite(t *testing.T) {
	oldInterval := delegationPollInterval
	delegationPollInterval = time.Millisecond
	t.Cleanup(func() { delegationPollInterval = oldInterval })

	schedulerDir := t.TempDir()
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const requestID = "torn-req"
	respPath := filepath.Join(reqDir, requestID+responseSuffix)

	// Land a torn (unparseable) response first — what a reader catches mid-write.
	if err := os.WriteFile(respPath, []byte(`{"runId":`), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var gotID string
	var gotErr error
	go func() {
		gotID, gotErr = pollTriggerResponse(context.Background(), schedulerDir, requestID, testResponseWait)
		close(done)
	}()

	// Give the poller time to observe the torn file at least once, then complete
	// the write. A correct poller re-polls and only consumes a parseable file.
	time.Sleep(20 * time.Millisecond)
	data, err := json.Marshal(triggerResponse{RunID: "run-xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(respPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pollTriggerResponse did not return after the complete response was written")
	}
	if gotErr != nil {
		t.Fatalf("pollTriggerResponse errored on a torn-then-complete write: %v", gotErr)
	}
	if gotID != "run-xyz" {
		t.Fatalf("runID = %q, want %q", gotID, "run-xyz")
	}
	if _, err := os.Stat(respPath); !os.IsNotExist(err) {
		t.Errorf("response file not consumed after a successful parse (stat err = %v)", err)
	}
}

// blockingDelegateStarter holds its Start call until release is closed, so a
// test can keep a workflow's max-parallel slot occupied for as long as it
// needs to.
type blockingDelegateStarter struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func newBlockingDelegateStarter() *blockingDelegateStarter {
	return &blockingDelegateStarter{release: make(chan struct{}), entered: make(chan struct{}, 1)}
}

func (b *blockingDelegateStarter) Start(ctx context.Context, _ localscheduler.StartRequest) (localscheduler.StartResult, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return localscheduler.StartResult{Phase: journal.PhaseCompleted}, nil
}

func (b *blockingDelegateStarter) releaseAll() { b.once.Do(func() { close(b.release) }) }

// TestSweepRequeuesTriggerRefusedForCapacity is the #874 regression guard. A
// max-parallel refusal is transient by construction: dispatch releases a
// workflow's slot in a deferred call that runs *after* Starter.Start returns,
// while `goobers run` decides the run is over by watching the run's own
// journal — which the runner wrote from inside that call. So the slot is
// still held for a moment after the command that owned it has exited, and a
// second `goobers run` in that window used to come back as a hard error
// ("run conditions rejected the trigger ... conditions: max-parallel").
//
// The sweep must instead put the request back and retry on a later pass. This
// drives that directly: hold the slot, sweep, and require that the request
// survives with no response written.
func TestSweepRequeuesTriggerRefusedForCapacity(t *testing.T) {
	blocking := newBlockingDelegateStarter()
	t.Cleanup(blocking.releaseAll)
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 10},
		Starter:   blocking,
	}})

	occupyID, err := writeTriggerRequest(schedulerDir, "implement")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now); err != nil {
		t.Fatalf("occupying sweepPendingTriggers: %v", err)
	}
	if _, err := pollTriggerResponse(context.Background(), schedulerDir, occupyID, testResponseWait); err != nil {
		t.Fatalf("occupying pollTriggerResponse: %v", err)
	}
	select {
	case <-blocking.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("occupying run never entered Start; its slot is not held")
	}

	contendedID, err := writeTriggerRequest(schedulerDir, "implement")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	reqPath := filepath.Join(schedulerDir, pendingTriggersDir, contendedID+requestSuffix)
	respPath := filepath.Join(schedulerDir, pendingTriggersDir, contendedID+responseSuffix)
	if err := sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now); err != nil {
		t.Fatalf("contended sweepPendingTriggers: %v", err)
	}
	if _, err := os.Stat(respPath); !os.IsNotExist(err) {
		data, _ := os.ReadFile(respPath)
		t.Fatalf("capacity refusal answered the client instead of requeueing: response = %q (stat err = %v)", data, err)
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Fatalf("contended request was consumed rather than requeued: %v", err)
	}

	// Freeing the slot lets an ordinary later sweep dispatch the very same
	// request — no client-visible failure anywhere in the sequence.
	blocking.releaseAll()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now); err != nil {
			t.Fatalf("retry sweepPendingTriggers: %v", err)
		}
		if _, err := os.Stat(respPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("requeued request was never dispatched after the slot freed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	runID, err := pollTriggerResponse(context.Background(), schedulerDir, contendedID, testResponseWait)
	if err != nil {
		t.Fatalf("requeued pollTriggerResponse: %v", err)
	}
	if runID == "" {
		t.Fatal("expected a non-empty run id for the requeued request")
	}
}

// TestSweepFailsFastOnNonTransientRefusal is the other half of the contract:
// only capacity refusals requeue. A spent budget cannot clear by waiting, so
// requeueing one would trade a clear error for a silent 30s hang before the
// staleness check finally answered.
func TestSweepFailsFastOnNonTransientRefusal(t *testing.T) {
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 1},
		Starter:   starter,
	}})

	// Spend the hourly budget.
	firstID, err := writeTriggerRequest(schedulerDir, "implement")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now); err != nil {
		t.Fatalf("first sweepPendingTriggers: %v", err)
	}
	if _, err := pollTriggerResponse(context.Background(), schedulerDir, firstID, testResponseWait); err != nil {
		t.Fatalf("first pollTriggerResponse: %v", err)
	}
	// The first request's RESPONSE lands as soon as Trigger returns a run id,
	// which is strictly before that run's slot is released: dispatch hands the
	// Starter call to a goroutine and only its `defer ReleaseWorkflow` frees
	// the max-parallel slot (see TriggerRejectedError.Transient's doc). So
	// polling the response above proves nothing about capacity. Without this
	// Wait the second trigger races that release and, whenever the goroutine
	// is slow to be scheduled (ordinary CI load), is refused for max-parallel
	// — a TRANSIENT reason, which the sweep requeues rather than answering,
	// leaving this test to burn the full testResponseWait failsafe and fail
	// on a timeout instead of the budget refusal it is actually about
	// (#958/#962). Waiting for the dispatch to finish makes the slot
	// deterministically free, so the only refusal left to observe is the
	// spent hourly budget.
	sched.Wait()

	secondID, err := writeTriggerRequest(schedulerDir, "implement")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, sched, time.Now); err != nil {
		t.Fatalf("second sweepPendingTriggers: %v", err)
	}
	_, err = pollTriggerResponse(context.Background(), schedulerDir, secondID, testResponseWait)
	if err == nil {
		t.Fatal("expected the budget refusal to be reported, not requeued")
	}
	if !strings.Contains(err.Error(), "run conditions rejected the trigger") {
		t.Fatalf("err = %v, want the run-conditions rejection", err)
	}
}
