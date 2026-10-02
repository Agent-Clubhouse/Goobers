package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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

	mu       sync.Mutex
	calls    int
	requests []localscheduler.StartRequest
	started  chan struct{}
}

func (f *fakeDelegateStarter) Start(_ context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.requests = append(f.requests, req)
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	return f.result, f.err
}

func (f *fakeDelegateStarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeDelegateStarter) lastRequest() localscheduler.StartRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func newTestDelegateScheduler(t *testing.T, entries []localscheduler.WorkflowEntry, opts ...localscheduler.Option) (*localscheduler.Scheduler, string) {
	t.Helper()
	dir := t.TempDir()
	log, _, err := journal.OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return localscheduler.New(entries, log, opts...), dir
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

	requestID, err := writeTriggerRequestContext(context.Background(), schedulerDir, "", "implement")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}

	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
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
	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	var recovered bool
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation &&
			event.RunID == runID &&
			event.Runner["kind"] == journal.RunnerAnnotationTriggerRecovery &&
			event.Runner["action"] == journal.RecoveryActionNewClaim &&
			event.Runner["requestId"] == requestID {
			recovered = true
		}
	}
	if !recovered {
		t.Fatalf("events = %+v, want pending trigger new-claim annotation", events)
	}
}

func TestRunDelegatedTargetedPullRequestDispatchesExactReference(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	ctx, cancel := context.WithTimeout(context.Background(), testResponseWait)
	defer cancel()

	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched := localscheduler.New([]localscheduler.WorkflowEntry{{
		Workflow: "merge-review",
		Signals:  []string{"github-webhook:pull_request"},
		Starter:  starter,
	}}, log, localscheduler.WithTargetedPRValidator(func(_ context.Context, _ localscheduler.WorkflowEntry, number int) error {
		if number != 3261 {
			return fmt.Errorf("pull request number = %d, want 3261", number)
		}
		return nil
	}))

	var stdout, stderr bytes.Buffer
	codeDone := make(chan int, 1)
	go func() {
		codeDone <- runDelegatedTrigger(ctx, l, runTarget{Workflow: "merge-review", PR: 3261}, root, true, &stdout, &stderr)
	}()

	requestDir := filepath.Join(l.SchedulerDir(), pendingTriggersDir)
	var requestPath string
	for requestPath == "" {
		entries, readErr := os.ReadDir(requestDir)
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), requestSuffix) {
				requestPath = filepath.Join(requestDir, entry.Name())
				break
			}
		}
		if requestPath != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	var data []byte
	for len(data) == 0 {
		data, err = os.ReadFile(requestPath)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(err)
		case <-time.After(time.Millisecond):
		}
	}
	var req triggerRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	if req.Workflow != "merge-review" || req.PR != 3261 {
		t.Fatalf("request = %+v, want merge-review PR 3261", req)
	}
	if err := sweepPendingTriggers(ctx, l.SchedulerDir(), nil, sched, time.Now); err != nil {
		t.Fatal(err)
	}
	var code int
	select {
	case code = <-codeDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	sched.Wait()
	trigger := starter.lastRequest().Trigger
	if trigger.Kind != journal.TriggerSignal || trigger.Ref != "github-webhook:pull_request#3261" {
		t.Fatalf("trigger = %+v, want exact targeted pull-request reference", trigger)
	}
	if !strings.Contains(stdout.String(), "created run ") {
		t.Fatalf("stdout = %q, want delegated run id", stdout.String())
	}
}

func TestDelegatedTargetValidationDeadlinePreventsLateDispatch(t *testing.T) {
	oldTimeout := triggerDelegationTimeout
	oldPollInterval := delegationPollInterval
	oldNow := delegationNow
	triggerDelegationTimeout = 500 * time.Millisecond
	delegationPollInterval = time.Millisecond
	nowMu := sync.Mutex{}
	now := time.Now()
	delegationNow = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	t.Cleanup(func() {
		triggerDelegationTimeout = oldTimeout
		delegationPollInterval = oldPollInterval
		delegationNow = oldNow
	})

	root := t.TempDir()
	l := instance.NewLayout(root)
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	validationStarted := make(chan struct{})
	releaseValidation := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseValidation) }) }
	t.Cleanup(release)
	sched := localscheduler.New([]localscheduler.WorkflowEntry{{
		Workflow: "merge-review",
		Signals:  []string{"github-webhook:pull_request"},
		Starter:  starter,
	}}, log,
		localscheduler.WithClock(delegationNow, time.After),
		localscheduler.WithTargetedPRValidator(func(_ context.Context, _ localscheduler.WorkflowEntry, _ int) error {
			select {
			case <-validationStarted:
			default:
				close(validationStarted)
			}
			<-releaseValidation
			// Deliberately ignore cancellation. TriggerSignalExact must still check
			// the request context before dispatching.
			return nil
		}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	codeDone := make(chan int, 1)
	go func() {
		codeDone <- runDelegatedTrigger(ctx, l, runTarget{Workflow: "merge-review", PR: 3261}, root, true, &stdout, &stderr)
	}()

	requestDir := filepath.Join(l.SchedulerDir(), pendingTriggersDir)
	var requestPath string
	for requestPath == "" {
		entries, readErr := os.ReadDir(requestDir)
		if readErr != nil && !os.IsNotExist(readErr) {
			t.Fatal(readErr)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), requestSuffix) {
				requestPath = filepath.Join(requestDir, entry.Name())
				break
			}
		}
		if requestPath == "" {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
	var requestData []byte
	for len(requestData) == 0 {
		requestData, err = os.ReadFile(requestPath)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(err)
		case <-time.After(time.Millisecond):
		}
	}
	var req triggerRequest
	if err := json.Unmarshal(requestData, &req); err != nil {
		t.Fatal(err)
	}
	if !req.Deadline.IsZero() {
		t.Fatalf("request deadline = %s, want no serialized default deadline for daemon-owned validation", req.Deadline)
	}

	sweepDone := make(chan error, 1)
	go func() {
		sweepDone <- sweepPendingTriggers(context.Background(), l.SchedulerDir(), nil, sched, delegationNow)
	}()
	select {
	case <-validationStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	nowMu.Lock()
	now = now.Add(triggerResponseWait() + time.Millisecond)
	nowMu.Unlock()

	release()
	select {
	case err := <-sweepDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var code int
	select {
	case code = <-codeDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if code == 0 || strings.Contains(stdout.String(), "state=queued") || !strings.Contains(stderr.String(), context.DeadlineExceeded.Error()) {
		t.Fatalf("delegated CLI result: code = %d, stdout = %q, stderr = %q; want deadline refusal without queued acceptance", code, stdout.String(), stderr.String())
	}
	sched.Wait()
	if starter.count() != 0 {
		t.Fatalf("starter calls = %d, want no dispatch after the client deadline", starter.count())
	}
	if _, err := os.Stat(requestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed request stat error = %v, want no replayable request", err)
	}
	if err := sweepPendingTriggers(context.Background(), l.SchedulerDir(), nil, sched, time.Now); err != nil {
		t.Fatal(err)
	}
	if starter.count() != 0 {
		t.Fatalf("starter calls after second sweep = %d, want no duplicate late dispatch", starter.count())
	}
}

func TestSweepDispatchesGaggleQualifiedRequest(t *testing.T) {
	alpha := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	beta := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{
		{Gaggle: "alpha", Workflow: "deploy", Starter: alpha},
		{Gaggle: "beta", Workflow: "deploy", Starter: beta},
	})

	requestID, err := writeTriggerRequestContext(context.Background(), schedulerDir, "beta", "deploy")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}
	if _, err := pollTriggerResponse(context.Background(), schedulerDir, requestID, testResponseWait); err != nil {
		t.Fatalf("pollTriggerResponse: %v", err)
	}
	sched.Wait()
	if alpha.count() != 0 || beta.count() != 1 {
		t.Fatalf("starter calls: alpha=%d beta=%d, want alpha=0 beta=1", alpha.count(), beta.count())
	}

	requestID, err = writeTriggerRequestContext(context.Background(), schedulerDir, "gamma", "deploy")
	if err != nil {
		t.Fatalf("writeTriggerRequest unknown gaggle: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
		t.Fatalf("sweepPendingTriggers unknown gaggle: %v", err)
	}
	if _, err := pollTriggerResponse(context.Background(), schedulerDir, requestID, testResponseWait); err == nil ||
		!strings.Contains(err.Error(), `unknown workflow "deploy" in gaggle "gamma"`) {
		t.Fatalf("unknown gaggle error = %v", err)
	}
}

func TestPriorityTriggerDispatchesExactWorkflowWithoutResponse(t *testing.T) {
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	other := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{
		{
			Workflow:  "merge-review",
			Gaggle:    "goobers",
			Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Starter:   starter,
		},
		{
			Workflow:  "merge-review",
			Gaggle:    "other",
			Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
			Starter:   other,
		},
	})

	requestID, err := writePriorityTriggerRequest(schedulerDir, "goobers", "merge-review", "source-run")
	if err != nil {
		t.Fatalf("writePriorityTriggerRequest: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, func() time.Time {
		return time.Now().Add(triggerDelegationTimeout + time.Second)
	}); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for starter.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond) // Polling interval for the test starter's synchronized call count.
	}
	if starter.count() != 1 {
		t.Fatalf("start calls = %d, want 1", starter.count())
	}
	if other.count() != 0 {
		t.Fatalf("other gaggle start calls = %d, want 0", other.count())
	}
	req := starter.lastRequest()
	if req.Trigger.Kind != journal.TriggerSignal || req.Trigger.Ref != "priority-re-tick:source-run" {
		t.Fatalf("trigger = %+v, want priority signal for source-run", req.Trigger)
	}
	responsePath := filepath.Join(schedulerDir, pendingTriggersDir, requestID+responseSuffix)
	if _, err := os.Stat(responsePath); !os.IsNotExist(err) {
		t.Fatalf("priority response file stat error = %v, want no response file", err)
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

	if _, err := writeTriggerRequestContext(context.Background(), schedulerDir, "", "implement"); err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
		t.Fatalf("first sweepPendingTriggers: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
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

func writeTriggerStateFixture(t *testing.T, schedulerDir, requestID, suffix string, req triggerRequest) {
	t.Helper()
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reqDir, requestID+suffix), data, 0o644); err != nil {
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
		Deadline:  now.Add(-2 * time.Second),
	})

	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, func() time.Time { return now }); err != nil {
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

func TestStartupSweepRefusesLegacyNoDeadlineRequestAsStale(t *testing.T) {
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1},
		Starter:   starter,
	}})
	now := time.Date(2026, 9, 29, 23, 30, 0, 0, time.UTC)
	const requestID = "legacy-stale"
	writeTriggerRequestFixture(t, schedulerDir, requestID, triggerRequest{
		Workflow:  "no-such-workflow",
		CreatedAt: now.Add(-triggerDelegationTimeout - time.Second),
	})

	err := sweepPendingTriggersWithOptions(context.Background(), schedulerDir, nil, sched, func() time.Time { return now }, triggerSweepOptions{
		staleLegacyMissingDeadline: true,
	})
	if err != nil {
		t.Fatalf("sweepPendingTriggersWithOptions: %v", err)
	}
	_, err = pollTriggerResponse(context.Background(), schedulerDir, requestID, testResponseWait)
	if err == nil || !strings.Contains(err.Error(), "stale trigger request") {
		t.Fatalf("pollTriggerResponse error = %v, want stale trigger request", err)
	}
	if strings.Contains(err.Error(), "unknown workflow") {
		t.Fatalf("startup stale request resolved workflow before stale refusal: %v", err)
	}
	if starter.count() != 0 {
		t.Fatalf("starter calls = %d, want 0", starter.count())
	}
}

func TestSweepGivesQueuedTargetedValidationAFreshAttemptBudget(t *testing.T) {
	oldTimeout := triggerDelegationTimeout
	triggerDelegationTimeout = 500 * time.Millisecond
	t.Cleanup(func() { triggerDelegationTimeout = oldTimeout })

	root := t.TempDir()
	l := instance.NewLayout(root)
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	queuedAt := time.Now().Add(-450 * time.Millisecond).UTC()
	sweepAt := time.Now().UTC()
	validationDeadline := make(chan time.Time, 1)
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched := localscheduler.New([]localscheduler.WorkflowEntry{{
		Workflow: "merge-review",
		Signals:  []string{"github-webhook:pull_request"},
		Starter:  starter,
	}}, log, localscheduler.WithTargetedPRValidator(func(ctx context.Context, _ localscheduler.WorkflowEntry, _ int) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("validation context has no deadline")
		}
		validationDeadline <- deadline
		return ctx.Err()
	}))

	const requestID = "queued-target"
	writeTriggerRequestFixture(t, l.SchedulerDir(), requestID, triggerRequest{
		Workflow:  "merge-review",
		PR:        3261,
		CreatedAt: queuedAt,
		Deadline:  queuedAt.Add(triggerDelegationTimeout),
	})

	if err := sweepPendingTriggers(context.Background(), l.SchedulerDir(), nil, sched, func() time.Time { return sweepAt }); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}
	gotDeadline := <-validationDeadline
	if gotDeadline.Before(sweepAt.Add(triggerDelegationTimeout)) {
		t.Fatalf("validation deadline = %s, want a fresh attempt budget from sweep time %s", gotDeadline, sweepAt)
	}
	sched.Wait()
	if starter.count() != 1 {
		t.Fatalf("starter calls = %d, want dispatch after fresh validation budget", starter.count())
	}
}

func TestSweepCapsTargetedValidationAtExplicitRequestDeadline(t *testing.T) {
	oldTimeout := triggerDelegationTimeout
	triggerDelegationTimeout = 500 * time.Millisecond
	t.Cleanup(func() { triggerDelegationTimeout = oldTimeout })

	root := t.TempDir()
	l := instance.NewLayout(root)
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	createdAt := time.Now().UTC()
	sweepAt := createdAt.Add(100 * time.Millisecond)
	explicitDeadline := sweepAt.Add(50 * time.Millisecond)
	validationDeadline := make(chan time.Time, 1)
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched := localscheduler.New([]localscheduler.WorkflowEntry{{
		Workflow: "merge-review",
		Signals:  []string{"github-webhook:pull_request"},
		Starter:  starter,
	}}, log, localscheduler.WithTargetedPRValidator(func(ctx context.Context, _ localscheduler.WorkflowEntry, _ int) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("validation context has no deadline")
		}
		validationDeadline <- deadline
		return ctx.Err()
	}))

	const requestID = "explicit-deadline-target"
	writeTriggerRequestFixture(t, l.SchedulerDir(), requestID, triggerRequest{
		Workflow:  "merge-review",
		PR:        3261,
		CreatedAt: createdAt,
		Deadline:  explicitDeadline,
	})

	if err := sweepPendingTriggers(context.Background(), l.SchedulerDir(), nil, sched, func() time.Time { return sweepAt }); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}
	if got := <-validationDeadline; !got.Equal(explicitDeadline) {
		t.Fatalf("validation deadline = %s, want explicit request deadline %s", got, explicitDeadline)
	}
}

func TestSweepCollectsExpiredOrphanResponse(t *testing.T) {
	sched, schedulerDir := newTestDelegateScheduler(t, nil)
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldResponsePath := filepath.Join(reqDir, "old"+responseSuffix)
	freshResponsePath := filepath.Join(reqDir, "fresh"+responseSuffix)
	oldAckPath := filepath.Join(reqDir, "old"+ackSuffix)
	freshAckPath := filepath.Join(reqDir, "fresh"+ackSuffix)
	oldAbandonedPath := filepath.Join(reqDir, "old"+abandonedSuffix)
	freshAbandonedPath := filepath.Join(reqDir, "fresh"+abandonedSuffix)
	for _, path := range []string{oldResponsePath, freshResponsePath} {
		if err := os.WriteFile(path, []byte(`{"runId":"orphan"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{oldAckPath, freshAckPath} {
		if err := os.WriteFile(path, []byte(`{"state":"queued"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{oldAbandonedPath, freshAbandonedPath} {
		if err := os.WriteFile(path, []byte(`{"workflow":"implement"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	old := now.Add(-triggerDelegationTimeout - time.Second)
	fresh := now.Add(-triggerDelegationTimeout + time.Second)
	for _, path := range []string{oldResponsePath, oldAckPath, oldAbandonedPath} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{freshResponsePath, freshAckPath, freshAbandonedPath} {
		if err := os.Chtimes(path, fresh, fresh); err != nil {
			t.Fatal(err)
		}
	}

	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, func() time.Time { return now }); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}

	for _, path := range []string{oldResponsePath, oldAckPath, oldAbandonedPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired orphan artifact %s stat error = %v, want not exist", filepath.Base(path), err)
		}
	}
	for _, path := range []string{freshResponsePath, freshAckPath, freshAbandonedPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("fresh artifact %s was removed: %v", filepath.Base(path), err)
		}
	}
}

func TestPollFinalResponseRemovesQueuedAck(t *testing.T) {
	schedulerDir := t.TempDir()
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const requestID = "final-before-ack"
	ackPath := filepath.Join(reqDir, requestID+ackSuffix)
	if err := os.WriteFile(ackPath, []byte(`{"state":"queued"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	responsePath := filepath.Join(reqDir, requestID+responseSuffix)
	if err := os.WriteFile(responsePath, []byte(`{"runId":"run-final"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := pollTriggerResponseEvent(context.Background(), schedulerDir, requestID, testResponseWait, false)
	if err != nil {
		t.Fatal(err)
	}
	if resp.RunID != "run-final" {
		t.Fatalf("runID = %q, want run-final", resp.RunID)
	}
	if _, err := os.Stat(ackPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ack stat error = %v, want removed with terminal response", err)
	}
}

func TestStartupSweepRecoversAcknowledgedActiveTrigger(t *testing.T) {
	started := make(chan struct{}, 1)
	starter := &fakeDelegateStarter{
		result:  localscheduler.StartResult{Phase: journal.PhaseCompleted},
		started: started,
	}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow: "implement",
		Starter:  starter,
	}})
	now := time.Now().UTC()
	const requestID = "acked-active"
	writeTriggerStateFixture(t, schedulerDir, requestID, activeSuffix, triggerRequest{
		Workflow:   "implement",
		CreatedAt:  now.Add(-time.Second),
		AcceptedAt: now.Add(-500 * time.Millisecond),
	})
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	ackPath := filepath.Join(reqDir, requestID+ackSuffix)
	if err := os.WriteFile(ackPath, []byte(`{"state":"queued"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := sweepPendingTriggersWithOptions(context.Background(), schedulerDir, nil, sched, func() time.Time { return now }, triggerSweepOptions{
		recoverActiveRequests: true,
	}); err != nil {
		t.Fatalf("startup sweep recovery: %v", err)
	}
	runID, err := pollTriggerResponse(context.Background(), schedulerDir, requestID, testResponseWait)
	if err != nil {
		t.Fatalf("poll recovered response: %v", err)
	}
	if runID == "" {
		t.Fatal("recovered active trigger returned an empty run id")
	}
	select {
	case <-started:
	case <-time.After(testResponseWait):
		t.Fatal("timed out waiting for recovered dispatch to reach the starter")
	}
	if starter.count() != 1 {
		t.Fatalf("starter calls = %d, want recovered dispatch", starter.count())
	}
	for _, suffix := range []string{requestSuffix, activeSuffix, ackSuffix} {
		path := filepath.Join(reqDir, requestID+suffix)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s stat error = %v, want removed after recovered terminal response", filepath.Base(path), err)
		}
	}
}

func TestStartupSweepAnswersDispatchedActiveTriggerWithoutReplay(t *testing.T) {
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow: "implement",
		Starter:  starter,
	}})
	root := filepath.Dir(schedulerDir)
	now := time.Now().UTC()
	const requestID = "dispatched-active"
	const runID = "11111111111111111111111111111111"
	writeStatusRunWithPhase(t, root, runID, "implement", "example", now, journal.PhaseRunning)
	writeTriggerStateFixture(t, schedulerDir, requestID, activeSuffix, triggerRequest{
		Workflow:      "implement",
		CreatedAt:     now.Add(-time.Second),
		AcceptedAt:    now.Add(-500 * time.Millisecond),
		DispatchRunID: runID,
	})
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	ackPath := filepath.Join(reqDir, requestID+ackSuffix)
	if err := os.WriteFile(ackPath, []byte(`{"state":"queued"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := sweepPendingTriggersWithOptions(context.Background(), schedulerDir, nil, sched, func() time.Time { return now }, triggerSweepOptions{
		recoverActiveRequests: true,
	}); err != nil {
		t.Fatalf("startup sweep recovery: %v", err)
	}
	gotRunID, err := pollTriggerResponse(context.Background(), schedulerDir, requestID, testResponseWait)
	if err != nil {
		t.Fatalf("poll recovered response: %v", err)
	}
	if gotRunID != runID {
		t.Fatalf("runID = %q, want recovered dispatched run %q", gotRunID, runID)
	}
	if starter.count() != 0 {
		t.Fatalf("starter calls = %d, want no replay after dispatch marker", starter.count())
	}
	for _, suffix := range []string{requestSuffix, activeSuffix, ackSuffix} {
		path := filepath.Join(reqDir, requestID+suffix)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s stat error = %v, want removed after recovered terminal response", filepath.Base(path), err)
		}
	}
}

func TestStartupSweepRefusesAmbiguousDispatchMarkerWithoutReplay(t *testing.T) {
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow: "implement",
		Starter:  starter,
	}})
	now := time.Now().UTC()
	const requestID = "ambiguous-active"
	const runID = "22222222222222222222222222222222"
	writeTriggerStateFixture(t, schedulerDir, requestID, activeSuffix, triggerRequest{
		Workflow:      "implement",
		CreatedAt:     now.Add(-time.Second),
		AcceptedAt:    now.Add(-500 * time.Millisecond),
		DispatchRunID: runID,
	})

	if err := sweepPendingTriggersWithOptions(context.Background(), schedulerDir, nil, sched, func() time.Time { return now }, triggerSweepOptions{
		recoverActiveRequests: true,
	}); err != nil {
		t.Fatalf("startup sweep recovery: %v", err)
	}
	_, err := pollTriggerResponse(context.Background(), schedulerDir, requestID, testResponseWait)
	if err == nil || !strings.Contains(err.Error(), "refusing to replay") {
		t.Fatalf("poll recovered response error = %v, want fail-closed replay refusal", err)
	}
	if starter.count() != 0 {
		t.Fatalf("starter calls = %d, want no replay after ambiguous dispatch marker", starter.count())
	}
}

func TestRunDelegatedNoWaitValidationFailuresReturnNonZero(t *testing.T) {
	for _, tt := range []struct {
		name    string
		target  runTarget
		entries []localscheduler.WorkflowEntry
		opts    []localscheduler.Option
		wantErr string
	}{
		{
			name:    "unknown workflow",
			target:  runTarget{Workflow: "missing"},
			wantErr: "unknown workflow",
		},
		{
			name:   "provider validation",
			target: runTarget{Workflow: "merge-review", PR: 3261},
			entries: []localscheduler.WorkflowEntry{{
				Workflow: "merge-review",
				Signals:  []string{"github-webhook:pull_request"},
				Starter:  &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}},
			}},
			opts: []localscheduler.Option{localscheduler.WithTargetedPRValidator(func(context.Context, localscheduler.WorkflowEntry, int) error {
				return errors.New("pull request validation failed")
			})},
			wantErr: "pull request validation failed",
		},
		{
			name:   "terminal readiness refusal",
			target: runTarget{Workflow: "implement"},
			entries: []localscheduler.WorkflowEntry{{
				Workflow:  "implement",
				Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 1},
				Starter:   &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}},
			}},
			wantErr: "run conditions rejected",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			l := instance.NewLayout(root)
			log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = log.Close() })
			sched := localscheduler.New(tt.entries, log, tt.opts...)
			if tt.name == "terminal readiness refusal" {
				firstID, err := writeTriggerRequestContext(context.Background(), l.SchedulerDir(), "", "implement")
				if err != nil {
					t.Fatal(err)
				}
				if err := sweepPendingTriggers(context.Background(), l.SchedulerDir(), nil, sched, time.Now); err != nil {
					t.Fatal(err)
				}
				if _, err := pollTriggerResponse(context.Background(), l.SchedulerDir(), firstID, testResponseWait); err != nil {
					t.Fatal(err)
				}
				sched.Wait()
			}

			ctx, cancel := context.WithTimeout(context.Background(), testResponseWait)
			defer cancel()
			var stdout, stderr bytes.Buffer
			codeDone := make(chan int, 1)
			go func() {
				codeDone <- runDelegatedTrigger(ctx, l, tt.target, root, true, &stdout, &stderr)
			}()
			waitForDelegatedRequestID(t, ctx, l.SchedulerDir())
			var code int
			for {
				if err := sweepPendingTriggers(context.Background(), l.SchedulerDir(), nil, sched, time.Now); err != nil && !strings.Contains(err.Error(), "read trigger request") {
					t.Fatal(err)
				}
				select {
				case code = <-codeDone:
					goto answered
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				default:
					runtime.Gosched()
				}
			}
		answered:
			if code == 0 {
				t.Fatalf("code = 0, want non-zero; stdout = %q stderr = %q", stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String(), "state=queued") {
				t.Fatalf("stdout = %q, validation failure must not produce queued acceptance", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

// TestSweepUnknownWorkflowRespondsWithError proves a delegated request for a
// workflow that doesn't exist surfaces the same "unknown workflow" error
// Scheduler.Trigger itself returns — through the response file, not silently
// dropped.
func TestSweepUnknownWorkflowRespondsWithError(t *testing.T) {
	sched, schedulerDir := newTestDelegateScheduler(t, nil)

	requestID, err := writeTriggerRequestContext(context.Background(), schedulerDir, "", "no-such-workflow")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
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
	requestID, err := writeTriggerRequestContext(context.Background(), schedulerDir, "", "implement")
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
	for _, duration := range []string{"", "1h"} {
		t.Run("maxRunDuration="+duration, func(t *testing.T) {
			t.Run("automatic API", func(t *testing.T) { testRunDelegatesToLiveDaemon(t, false, duration) })
			t.Run("explicit file fallback", func(t *testing.T) { testRunDelegatesToLiveDaemon(t, true, duration) })
		})
	}
}

func testRunDelegatesToLiveDaemon(t *testing.T, noAPI bool, maxRunDuration string) {
	prevInterval := delegationSweepInterval
	delegationSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() { delegationSweepInterval = prevInterval })

	root := initDeterministicDemo(t)
	if maxRunDuration != "" {
		setRunDurationLimit(t, root, "workflow", maxRunDuration)
	}
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

	args := []string{"run", "default-implement", root}
	if noAPI {
		args = append(args, "--no-api")
	}
	code, stdout, stderr := runArgs(t, args...)
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
	fields := strings.Fields(stdout)
	if len(fields) < 3 || fields[0] != "accepted" || fields[1] != "trigger" {
		t.Fatalf("stdout = %q, want durable acceptance without waiting for dispatch", stdout)
	}
	if strings.Contains(stdout, "finished:") {
		t.Fatalf("stdout = %q, --no-wait must not report a terminal phase", stdout)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	endpoint, err := localDaemonAPIBase(l)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := waitAcceptedTriggerDispatch(waitCtx, endpoint, fields[2])
	if err != nil || accepted.State != "dispatched" {
		t.Fatalf("dispatch=%+v err=%v", accepted, err)
	}
	runID := accepted.RunID
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
	time.Sleep(20 * time.Millisecond) // Intentional torn-write window verifies parse errors are retried.
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
	mu      sync.Mutex
	calls   int
}

func newBlockingDelegateStarter() *blockingDelegateStarter {
	return &blockingDelegateStarter{release: make(chan struct{}), entered: make(chan struct{}, 1)}
}

func (b *blockingDelegateStarter) Start(ctx context.Context, _ localscheduler.StartRequest) (localscheduler.StartResult, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
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

func (b *blockingDelegateStarter) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
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
// The sweep must instead acknowledge the request, put it back, and retry on a
// later pass. This drives that directly: hold the slot, sweep, and require that
// the request survives with a queued acknowledgement rather than a hard error.
func TestSweepRequeuesTriggerRefusedForCapacity(t *testing.T) {
	blocking := newBlockingDelegateStarter()
	t.Cleanup(blocking.releaseAll)
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 10},
		Starter:   blocking,
	}})

	occupyID, err := writeTriggerRequestContext(context.Background(), schedulerDir, "", "implement")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
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

	contendedID, err := writeTriggerRequestContext(context.Background(), schedulerDir, "", "implement")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	reqPath := filepath.Join(schedulerDir, pendingTriggersDir, contendedID+requestSuffix)
	respPath := filepath.Join(schedulerDir, pendingTriggersDir, contendedID+responseSuffix)
	originalContendedData, err := os.ReadFile(reqPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
		t.Fatalf("contended sweepPendingTriggers: %v", err)
	}
	resp, err := pollTriggerResponseEvent(context.Background(), schedulerDir, contendedID, testResponseWait, false)
	if err != nil {
		t.Fatalf("queued acknowledgement: %v", err)
	}
	if resp.State != triggerResponseQueued {
		t.Fatalf("capacity response = %+v, want queued acknowledgement", resp)
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Fatalf("contended request was consumed rather than requeued: %v", err)
	}
	requeuedData, err := os.ReadFile(reqPath)
	if err != nil {
		t.Fatal(err)
	}
	var requeued triggerRequest
	if err := json.Unmarshal(requeuedData, &requeued); err != nil {
		t.Fatal(err)
	}
	var originalContended triggerRequest
	if err := json.Unmarshal(originalContendedData, &originalContended); err != nil {
		t.Fatal(err)
	}
	if !requeued.CreatedAt.Equal(originalContended.CreatedAt) {
		t.Fatalf("requeued CreatedAt = %s, want original queue order timestamp %s preserved", requeued.CreatedAt, originalContended.CreatedAt)
	}
	if requeued.AcceptedAt.IsZero() {
		t.Fatal("requeued request did not record acceptedAt")
	}

	// Freeing the slot lets an ordinary later sweep dispatch the very same
	// request — no client-visible failure anywhere in the sequence.
	blocking.releaseAll()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
			t.Fatalf("retry sweepPendingTriggers: %v", err)
		}
		if _, err := os.Stat(respPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("requeued request was never dispatched after the slot freed")
		}
		time.Sleep(10 * time.Millisecond) // Polling interval for the requeued request's journal event.
	}
	runID, err := pollTriggerResponse(context.Background(), schedulerDir, contendedID, testResponseWait)
	if err != nil {
		t.Fatalf("requeued pollTriggerResponse: %v", err)
	}
	if runID == "" {
		t.Fatal("expected a non-empty run id for the requeued request")
	}
}

func TestSweepDispatchesAcknowledgedCapacityRequeueAfterClientWait(t *testing.T) {
	for _, tt := range []struct {
		name     string
		deadline func(time.Time) time.Time
	}{
		{name: "no serialized deadline"},
		{name: "legacy default deadline", deadline: func(createdAt time.Time) time.Time {
			return createdAt.Add(triggerDelegationTimeout)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testSweepDispatchesAcknowledgedCapacityRequeueAfterClientWait(t, tt.deadline)
		})
	}
}

func testSweepDispatchesAcknowledgedCapacityRequeueAfterClientWait(t *testing.T, deadline func(time.Time) time.Time) {
	t.Helper()
	blocking := newBlockingDelegateStarter()
	t.Cleanup(blocking.releaseAll)
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 10},
		Starter:   blocking,
	}})
	queuedAt := time.Now().UTC()

	const occupyID = "occupy-capacity"
	writeTriggerRequestFixture(t, schedulerDir, occupyID, triggerRequest{
		Workflow:  "implement",
		CreatedAt: queuedAt,
	})
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, func() time.Time { return queuedAt }); err != nil {
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

	const contendedID = "client-wait-expired"
	contended := triggerRequest{
		Workflow:  "implement",
		CreatedAt: queuedAt,
	}
	if deadline != nil {
		contended.Deadline = deadline(queuedAt)
	}
	writeTriggerRequestFixture(t, schedulerDir, contendedID, contended)
	reqPath := filepath.Join(schedulerDir, pendingTriggersDir, contendedID+requestSuffix)
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, func() time.Time {
		return queuedAt.Add(time.Second)
	}); err != nil {
		t.Fatalf("contended sweepPendingTriggers: %v", err)
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Fatalf("contended request was not requeued: %v", err)
	}
	resp, err := pollTriggerResponseEvent(context.Background(), schedulerDir, contendedID, testResponseWait, false)
	if err != nil {
		t.Fatalf("queued acknowledgement: %v", err)
	}
	if resp.State != triggerResponseQueued {
		t.Fatalf("contended response = %+v, want queued acknowledgement", resp)
	}

	blocking.releaseAll()
	expiredAt := queuedAt.Add(triggerResponseWait() + time.Millisecond)
	deadlineAt := time.Now().Add(5 * time.Second)
	for {
		if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, func() time.Time { return expiredAt }); err != nil {
			t.Fatalf("expired requeue sweepPendingTriggers: %v", err)
		}
		if _, err := os.Stat(reqPath); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadlineAt) {
			t.Fatal("acknowledged requeued request was never dispatched after the slot freed")
		}
		time.Sleep(10 * time.Millisecond) // Polling interval for the scheduler's slot release.
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Fatalf("dispatched requeued request file survived: %v", err)
	}
	runID, err := pollTriggerResponse(context.Background(), schedulerDir, contendedID, testResponseWait)
	if err != nil {
		t.Fatalf("requeued pollTriggerResponse: %v", err)
	}
	if runID == "" {
		t.Fatal("requeued request produced an empty run id")
	}

	if blocking.count() != 2 {
		t.Fatalf("starter calls after queued dispatch = %d, want occupying run plus one queued dispatch", blocking.count())
	}
}

func TestPollWithdrawsUnacknowledgedTriggerAfterClientWait(t *testing.T) {
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow: "implement",
		Starter:  starter,
	}})
	const requestID = "unacknowledged"
	writeTriggerRequestFixture(t, schedulerDir, requestID, triggerRequest{
		Workflow:  "implement",
		CreatedAt: time.Now().UTC(),
	})

	_, err := pollTriggerResponseEvent(context.Background(), schedulerDir, requestID, -time.Nanosecond, true)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("withdraw poll error = %v, want timeout", err)
	}
	if _, err := os.Stat(filepath.Join(schedulerDir, pendingTriggersDir, requestID+requestSuffix)); !os.IsNotExist(err) {
		t.Fatalf("request file still dispatchable after withdraw: %v", err)
	}
	if _, err := os.Stat(filepath.Join(schedulerDir, pendingTriggersDir, requestID+abandonedSuffix)); err != nil {
		t.Fatalf("abandoned tombstone missing: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}
	if starter.count() != 0 {
		t.Fatalf("withdrawn request dispatched; starter calls = %d", starter.count())
	}
}

func TestTriggerAckWithdrawRaceHasSingleWinner(t *testing.T) {
	t.Run("withdraw wins", func(t *testing.T) {
		schedulerDir := t.TempDir()
		const requestID = "withdraw-wins"
		writeTriggerRequestFixture(t, schedulerDir, requestID, triggerRequest{
			Workflow: "implement", CreatedAt: time.Now().UTC(),
		})
		withdrawn, err := withdrawTriggerRequest(schedulerDir, requestID)
		if err != nil || !withdrawn {
			t.Fatalf("withdraw = %v, %v; want success", withdrawn, err)
		}
		reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
		if _, claimed, err := claimTriggerRequest(reqDir, requestID); err != nil || claimed {
			t.Fatalf("claim after withdraw = %v, %v; want no claim", claimed, err)
		}
	})
	t.Run("ack wins", func(t *testing.T) {
		schedulerDir := t.TempDir()
		const requestID = "ack-wins"
		writeTriggerRequestFixture(t, schedulerDir, requestID, triggerRequest{
			Workflow: "implement", CreatedAt: time.Now().UTC(),
		})
		reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
		activePath, claimed, err := claimTriggerRequest(reqDir, requestID)
		if err != nil || !claimed {
			t.Fatalf("claim = %v, %v; want success", claimed, err)
		}
		withdrawn, err := withdrawTriggerRequest(schedulerDir, requestID)
		if err != nil || withdrawn {
			t.Fatalf("withdraw after claim = %v, %v; want no withdraw", withdrawn, err)
		}
		req := triggerRequest{Workflow: "implement", CreatedAt: time.Now().UTC()}
		if err := acknowledgeTriggerRequest(reqDir, requestID, &req, time.Now().UTC()); err != nil {
			t.Fatalf("acknowledgeTriggerRequest: %v", err)
		}
		_ = os.Remove(activePath)
		resp, err := pollTriggerResponseEvent(context.Background(), schedulerDir, requestID, testResponseWait, true)
		if err != nil {
			t.Fatalf("poll queued ack: %v", err)
		}
		if resp.State != triggerResponseQueued {
			t.Fatalf("response = %+v, want queued ack", resp)
		}
	})
}

func TestPollWaitsForAckWhenClaimWinsWithdrawRace(t *testing.T) {
	oldNow := delegationNow
	oldPoll := delegationPollInterval
	oldGrace := triggerClaimAckGrace
	base := time.Now().UTC()
	var calls atomic.Int32
	graceSet := make(chan struct{})
	var closeGrace sync.Once
	delegationNow = func() time.Time {
		if calls.Add(1) == 3 {
			closeGrace.Do(func() { close(graceSet) })
		}
		return base
	}
	delegationPollInterval = time.Millisecond
	triggerClaimAckGrace = time.Hour
	t.Cleanup(func() {
		delegationNow = oldNow
		delegationPollInterval = oldPoll
		triggerClaimAckGrace = oldGrace
	})

	schedulerDir := t.TempDir()
	const requestID = "claim-wins-before-ack"
	writeTriggerRequestFixture(t, schedulerDir, requestID, triggerRequest{
		Workflow:  "implement",
		CreatedAt: base,
	})
	reqDir := filepath.Join(schedulerDir, pendingTriggersDir)
	activePath, claimed, err := claimTriggerRequest(reqDir, requestID)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v; want success", claimed, err)
	}
	t.Cleanup(func() { _ = os.Remove(activePath) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	var got triggerResponse
	var gotErr error
	go func() {
		got, gotErr = pollTriggerResponseEvent(ctx, schedulerDir, requestID, -time.Nanosecond, true)
		close(done)
	}()

	select {
	case <-graceSet:
	case <-ctx.Done():
		t.Fatal("poll did not enter claim-ack grace")
	}
	req := triggerRequest{Workflow: "implement", CreatedAt: base}
	if err := acknowledgeTriggerRequest(reqDir, requestID, &req, base); err != nil {
		t.Fatalf("acknowledgeTriggerRequest: %v", err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("poll did not return queued ack during claim-ack grace")
	}
	if gotErr != nil {
		t.Fatalf("poll error = %v, want queued ack", gotErr)
	}
	if got.State != triggerResponseQueued {
		t.Fatalf("response = %+v, want queued ack", got)
	}
}

func TestLateDelegatedPickupGetsFreshValidationBudget(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	sweepAt := time.Now().UTC()
	queuedAt := sweepAt.Add(-triggerResponseWait() - time.Hour)
	validationDeadline := make(chan time.Time, 1)
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched := localscheduler.New([]localscheduler.WorkflowEntry{{
		Workflow: "merge-review",
		Signals:  []string{"github-webhook:pull_request"},
		Starter:  starter,
	}}, log, localscheduler.WithTargetedPRValidator(func(ctx context.Context, _ localscheduler.WorkflowEntry, _ int) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("validation context has no deadline")
		}
		validationDeadline <- deadline
		return ctx.Err()
	}))

	const requestID = "late-pickup"
	writeTriggerRequestFixture(t, l.SchedulerDir(), requestID, triggerRequest{
		Workflow:  "merge-review",
		PR:        3261,
		CreatedAt: queuedAt,
	})
	if err := sweepPendingTriggers(context.Background(), l.SchedulerDir(), nil, sched, func() time.Time { return sweepAt }); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}
	gotDeadline := <-validationDeadline
	if gotDeadline.Before(sweepAt.Add(triggerDelegationTimeout)) {
		t.Fatalf("validation deadline = %s, want fresh attempt budget from %s", gotDeadline, sweepAt)
	}
}

func TestAcceptedTriggerQueueLifetimeExpiresRetryably(t *testing.T) {
	oldLifetime := acceptedTriggerQueueLifetime
	acceptedTriggerQueueLifetime = func() time.Duration { return time.Minute }
	t.Cleanup(func() { acceptedTriggerQueueLifetime = oldLifetime })

	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow: "implement",
		Starter:  starter,
	}})
	now := time.Now().UTC()
	const requestID = "accepted-expired"
	writeTriggerRequestFixture(t, schedulerDir, requestID, triggerRequest{
		Workflow:   "implement",
		CreatedAt:  now.Add(-2 * time.Minute),
		AcceptedAt: now.Add(-time.Minute - time.Second),
	})
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, func() time.Time { return now }); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}
	resp, err := pollTriggerResponseEvent(context.Background(), schedulerDir, requestID, testResponseWait, false)
	if err != nil {
		t.Fatalf("poll response: %v", err)
	}
	if resp.Error == "" || !resp.Retryable {
		t.Fatalf("response = %+v, want retryable terminal error", resp)
	}
	if starter.count() != 0 {
		t.Fatalf("expired accepted request dispatched; starter calls = %d", starter.count())
	}
}

// TestRequeueTriggerRequestNeverTornUnderConcurrentReads is #2022's
// regression guard: the capacity-refusal requeue path used to rewrite the
// live *.request.json in place with a plain os.WriteFile (O_TRUNC then
// write), so a sweep or concurrent inspection landing in that window could
// read empty or partial bytes and fail closed with "malformed trigger
// request" — recreating exactly the torn-read failure the request's initial
// atomic publish (writeTriggerRequestPayload) exists to prevent. It now goes
// through the same journal.WriteFileAtomic (hidden temp + rename) the
// sibling cancel protocol already used. A rename is atomic at the directory-
// entry level, so a concurrent reader observes either the complete old
// content or the complete new content, never a truncated mix — this drives
// many concurrent rewrites against a spinning reader and requires every
// single read to parse cleanly.
func TestRequeueTriggerRequestNeverTornUnderConcurrentReads(t *testing.T) {
	reqDir := t.TempDir()
	reqPath := filepath.Join(reqDir, "contended"+requestSuffix)

	data, err := json.Marshal(triggerRequest{Workflow: "implement", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteFileAtomic(reqPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	tornErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(reqPath)
			if err != nil {
				// The path always has a file at it (rename never leaves a gap);
				// a transient read error here is not the failure this test
				// guards against.
				continue
			}
			var got triggerRequest
			if jerr := json.Unmarshal(raw, &got); jerr != nil {
				select {
				case tornErr <- fmt.Errorf("torn read: %w (bytes=%q)", jerr, raw):
				default:
				}
				return
			}
		}
	}()

	// Mirrors the exact requeue call site (sweepPendingTriggers's capacity-
	// refusal branch): rewrite the same already-named request file in place,
	// repeatedly, while the reader above spins concurrently.
	for range 500 {
		if err := journal.WriteFileAtomic(reqPath, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()

	select {
	case err := <-tornErr:
		t.Fatal(err)
	default:
	}
}

func TestSweepForcedManualTriggerBypassesSpentBudget(t *testing.T) {
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 1},
		Starter:   starter,
	}})

	firstID, err := writeTriggerRequestContext(context.Background(), schedulerDir, "", "implement")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
		t.Fatalf("first sweepPendingTriggers: %v", err)
	}
	if _, err := pollTriggerResponse(context.Background(), schedulerDir, firstID, testResponseWait); err != nil {
		t.Fatalf("first pollTriggerResponse: %v", err)
	}
	sched.Wait()

	secondID, err := writeTriggerRequestContext(context.Background(), schedulerDir, "", "implement")
	if err != nil {
		t.Fatalf("writeTriggerRequest: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
		t.Fatalf("second sweepPendingTriggers: %v", err)
	}
	if _, err := pollTriggerResponse(context.Background(), schedulerDir, secondID, testResponseWait); err == nil {
		t.Fatal("default manual trigger bypassed maxRunsPerHour")
	}

	forcedID, err := writeTriggerRequestContextOptions(context.Background(), schedulerDir, "", "implement", true)
	if err != nil {
		t.Fatalf("write forced trigger request: %v", err)
	}
	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
		t.Fatalf("forced sweepPendingTriggers: %v", err)
	}
	if _, err := pollTriggerResponse(context.Background(), schedulerDir, forcedID, testResponseWait); err != nil {
		t.Fatalf("forced pollTriggerResponse: %v", err)
	}
	sched.Wait()
	if got := starter.count(); got != 2 {
		t.Fatalf("starter calls = %d, want default plus forced manual run", got)
	}
}

// TestSweepBoundsOutstandingDuplicateRequestsPerIdentity is #4326's core
// regression guard: a caller that drops many same-identity requests (the
// incident's "automation-fill-*" flood — a recurring automation that never
// accounted for already-pending requests) must not get every one of them
// dispatched. Only maxOutstandingTriggerRequestsPerIdentity are actually
// admitted through the scheduler; the rest are answered immediately as
// bounded-out, with no dispatch and no journaled refusal.
func TestSweepBoundsOutstandingDuplicateRequestsPerIdentity(t *testing.T) {
	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Gaggle:    "dogfood",
		Workflow:  "implementation",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 100},
		Starter:   starter,
	}})

	const flood = 20
	requestIDs := make([]string, 0, flood)
	for i := 0; i < flood; i++ {
		id, err := writeTargetedTriggerRequestContext(context.Background(), schedulerDir, "dogfood", "implementation", 0)
		if err != nil {
			t.Fatalf("writeTargetedTriggerRequestContext[%d]: %v", i, err)
		}
		requestIDs = append(requestIDs, id)
	}

	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}
	sched.Wait()

	if starter.count() != maxOutstandingTriggerRequestsPerIdentity {
		t.Fatalf("starter calls = %d, want bounded to %d", starter.count(), maxOutstandingTriggerRequestsPerIdentity)
	}

	var admitted, bounded int
	for _, id := range requestIDs {
		_, err := pollTriggerResponse(context.Background(), schedulerDir, id, testResponseWait)
		switch {
		case err == nil:
			admitted++
		case strings.Contains(err.Error(), "already outstanding") && strings.Contains(err.Error(), "rejected without dispatch"):
			bounded++
		default:
			t.Fatalf("pollTriggerResponse[%s]: unexpected error %v", id, err)
		}
	}
	if admitted != maxOutstandingTriggerRequestsPerIdentity {
		t.Fatalf("admitted = %d, want %d", admitted, maxOutstandingTriggerRequestsPerIdentity)
	}
	if bounded != flood-maxOutstandingTriggerRequestsPerIdentity {
		t.Fatalf("bounded = %d, want %d", bounded, flood-maxOutstandingTriggerRequestsPerIdentity)
	}

	entries, err := filepathGlobRequests(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected every flooded request consumed, found: %v", entries)
	}

	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	var refusals int
	for _, ev := range events {
		if ev.Type == journal.EventTickSkipped && ev.Workflow == "implementation" {
			refusals++
		}
	}
	if refusals != 0 {
		t.Fatalf("refusal journal events = %d, want 0 — bounded-out duplicates must not journal", refusals)
	}
}

// TestSweepAllowsDistinctIdentitiesPastThePerIdentityBound proves the bound
// in TestSweepBoundsOutstandingDuplicateRequestsPerIdentity is per-identity,
// not global: distinct (gaggle, workflow) targets each get their own budget,
// so five configured lanes each still admit up to the per-identity bound —
// exactly #4326's "five configured lanes produce at most five outstanding
// fill requests" acceptance criterion, generalized to whatever bound is
// configured.
func TestSweepAllowsDistinctIdentitiesPastThePerIdentityBound(t *testing.T) {
	alpha := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	beta := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{
		{Gaggle: "alpha", Workflow: "implementation", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 100}, Starter: alpha},
		{Gaggle: "beta", Workflow: "implementation", Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 100}, Starter: beta},
	})

	for i := 0; i < maxOutstandingTriggerRequestsPerIdentity; i++ {
		if _, err := writeTargetedTriggerRequestContext(context.Background(), schedulerDir, "alpha", "implementation", 0); err != nil {
			t.Fatalf("write alpha[%d]: %v", i, err)
		}
		if _, err := writeTargetedTriggerRequestContext(context.Background(), schedulerDir, "beta", "implementation", 0); err != nil {
			t.Fatalf("write beta[%d]: %v", i, err)
		}
	}

	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}
	sched.Wait()

	if alpha.count() != maxOutstandingTriggerRequestsPerIdentity || beta.count() != maxOutstandingTriggerRequestsPerIdentity {
		t.Fatalf("starter calls: alpha=%d beta=%d, want both = %d", alpha.count(), beta.count(), maxOutstandingTriggerRequestsPerIdentity)
	}
}

// TestSweepCapsEntriesExaminedPerCycle is #4323's direct regression guard for
// the incident scale (1,178 pending requests): one sweepPendingTriggers call
// must bound how much work it does regardless of backlog size, so a huge
// backlog drains progressively across ticks instead of stalling the
// delegation-ticker goroutine (and, behind it, claims processing and the
// scheduler's own tick) for the duration of one unbounded pass.
func TestSweepCapsEntriesExaminedPerCycle(t *testing.T) {
	oldCap := maxTriggerSweepEntriesPerCycle
	maxTriggerSweepEntriesPerCycle = 10
	t.Cleanup(func() { maxTriggerSweepEntriesPerCycle = oldCap })

	starter := &fakeDelegateStarter{result: localscheduler.StartResult{Phase: journal.PhaseCompleted}}
	sched, schedulerDir := newTestDelegateScheduler(t, []localscheduler.WorkflowEntry{{
		Workflow:  "implement",
		Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 100, MaxRunsPerHour: 1000},
		Starter:   starter,
	}})

	// The backlog is dropped in DIRECTLY rather than submitted through
	// writeTriggerRequestPayload, because that is how a backlog this deep
	// actually arrives: #4326's automation wrote request files itself, and the
	// supported writer now refuses past maxPendingTriggerRequests. Direct
	// drops are precisely the case this sweep bound must hold for — it cannot
	// assume anything about how a request file got there.
	const total = 1200
	for i := 0; i < total; i++ {
		writeTriggerRequestFixture(t, schedulerDir, fmt.Sprintf("direct-drop-%04d", i), triggerRequest{
			Workflow:  "implement",
			CreatedAt: time.Now(),
			Deadline:  time.Now().Add(triggerDelegationTimeout),
		})
	}

	if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
		t.Fatalf("sweepPendingTriggers: %v", err)
	}

	remaining, err := filepathGlobRequests(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != total-maxTriggerSweepEntriesPerCycle {
		t.Fatalf("remaining requests = %d, want %d left for the next cycle", len(remaining), total-maxTriggerSweepEntriesPerCycle)
	}

	// Draining fully takes ceil(total/cap) cycles; run the rest to prove the
	// backlog does converge to zero rather than getting stuck.
	for len(remaining) > 0 {
		if err := sweepPendingTriggers(context.Background(), schedulerDir, nil, sched, time.Now); err != nil {
			t.Fatalf("drain sweepPendingTriggers: %v", err)
		}
		remaining, err = filepathGlobRequests(schedulerDir)
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestPendingTriggerQueueStatsReportsDepthAndOldestAge is #4323's
// operator-visibility acceptance guard: `goobers status` must be able to see
// a growing pending-trigger backlog (and how stale its oldest entry is)
// before it starves anything, the way #4326's incident accumulated 1,177
// undetected duplicates.
func TestPendingTriggerQueueStatsReportsDepthAndOldestAge(t *testing.T) {
	schedulerDir := t.TempDir()

	depth, oldestAge, err := pendingTriggerQueueStats(schedulerDir, time.Now())
	if err != nil {
		t.Fatalf("pendingTriggerQueueStats on an instance that never delegated: %v", err)
	}
	if depth != 0 || oldestAge != 0 {
		t.Fatalf("depth = %d, oldestAge = %s, want 0/0 for no pending-triggers dir", depth, oldestAge)
	}

	oldID, err := writeTriggerRequestContext(context.Background(), schedulerDir, "", "implement")
	if err != nil {
		t.Fatalf("write old request: %v", err)
	}
	oldPath := filepath.Join(schedulerDir, pendingTriggersDir, oldID+requestSuffix)
	old := time.Now().Add(-90 * time.Second)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := writeTriggerRequestContext(context.Background(), schedulerDir, "", "implement"); err != nil {
		t.Fatalf("write fresh request: %v", err)
	}

	now := time.Now()
	depth, oldestAge, err = pendingTriggerQueueStats(schedulerDir, now)
	if err != nil {
		t.Fatalf("pendingTriggerQueueStats: %v", err)
	}
	if depth != 2 {
		t.Fatalf("depth = %d, want 2", depth)
	}
	if oldestAge < 89*time.Second || oldestAge > 100*time.Second {
		t.Fatalf("oldestAge = %s, want roughly 90s (the older request's age)", oldestAge)
	}
}
