package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/lock"
)

func TestClaimLockUncontendedIsLowNoise(t *testing.T) {
	schedulerDir := filepath.Join(t.TempDir(), "scheduler")
	lockPath := filepath.Join(schedulerDir, claimLockFileName)
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := withClaimLockThreshold(lockPath, "test.uncontended", time.Second, func() error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("uncontended lock emitted %d events, want none: %+v", len(events), events)
	}
}

func TestClaimLockDelayedHolderReportsSlowWaitAndOperation(t *testing.T) {
	schedulerDir := filepath.Join(t.TempDir(), "scheduler")
	lockPath := filepath.Join(schedulerDir, claimLockFileName)
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	holder, err := lock.Acquire(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	const (
		threshold = 10 * time.Millisecond
		holdFor   = 30 * time.Millisecond
		operation = claimLockOperationBacklogClaim
	)
	released := make(chan struct{})
	go func() {
		time.Sleep(holdFor)
		_ = holder.Release()
		close(released)
	}()

	if err := withClaimLockThreshold(lockPath, operation, threshold, func() error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-released

	event := readSingleSlowClaimLockEvent(t, schedulerDir, operation)
	waitDuration := claimLockEventDuration(t, event, "waitDuration")
	if waitDuration <= threshold {
		t.Fatalf("waitDuration = %s, want above %s", waitDuration, threshold)
	}
	_ = claimLockEventDuration(t, event, "holdDuration")
}

func TestClaimLockSlowHoldReportsBothDurations(t *testing.T) {
	schedulerDir := filepath.Join(t.TempDir(), "scheduler")
	lockPath := filepath.Join(schedulerDir, claimLockFileName)
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	const (
		threshold = 10 * time.Millisecond
		operation = claimLockOperationRunRelease
	)
	if err := withClaimLockThreshold(lockPath, operation, threshold, func() error {
		time.Sleep(30 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	event := readSingleSlowClaimLockEvent(t, schedulerDir, operation)
	_ = claimLockEventDuration(t, event, "waitDuration")
	holdDuration := claimLockEventDuration(t, event, "holdDuration")
	if holdDuration <= threshold {
		t.Fatalf("holdDuration = %s, want above %s", holdDuration, threshold)
	}
}

func TestClaimLockTimeoutIsJournaledRetryableAndDoesNotReleaseHolder(t *testing.T) {
	root := initDemo(t)
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunConditions.ClaimsLockTimeout = "20ms"
	if err := instance.WriteConfig(l.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}

	resultFile := filepath.Join(t.TempDir(), "stage-result.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)
	t.Setenv("GOOBERS_RUN_ID", "run-lock-timeout")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv("GOOBERS_GAGGLE", "example")

	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	holder, err := lock.Acquire(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	called := false
	started := time.Now()
	err = withClaimLock(lockPath, claimLockOperationBacklogClaim, func() error {
		called = true
		return nil
	})
	elapsed := time.Since(started)
	var timeoutErr *claimsLockTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v, want claimsLockTimeoutError", err)
	}
	if called {
		t.Fatal("timed-out caller ran the protected callback")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("lock acquisition took %s, want bounded near 20ms", elapsed)
	}

	if competing, err := lock.TryAcquire(lockPath); !errors.Is(err, lock.ErrHeld) {
		if err == nil {
			_ = competing.Release()
		}
		t.Fatalf("TryAcquire after timeout = %v, want ErrHeld", err)
	}

	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result[executor.OutputErrorCode] != claimsLockTimeoutCode {
		t.Fatalf("errorCode = %v, want %s", result[executor.OutputErrorCode], claimsLockTimeoutCode)
	}
	if result[executor.OutputErrorRetryable] != true {
		t.Fatalf("errorRetryable = %v, want true", result[executor.OutputErrorRetryable])
	}

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one timeout event", events)
	}
	event := events[0]
	if event.Type != journal.EventClaimLockTimeout || event.Error == nil || event.Error.Code != claimsLockTimeoutCode {
		t.Fatalf("timeout event = %+v", event)
	}
	if event.RunID != "run-lock-timeout" || event.Runner["retryable"] != true || event.Runner["failureClass"] != "infra" {
		t.Fatalf("timeout classification = %+v", event)
	}

	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	redispatched := false
	if err := withClaimLock(lockPath, claimLockOperationBacklogClaim, func() error {
		redispatched = true
		return nil
	}); err != nil {
		t.Fatalf("later dispatch did not acquire released lock: %v", err)
	}
	if !redispatched {
		t.Fatal("later dispatch did not run after actual holder released the lock")
	}
}

func TestClaimLockTimeoutWithoutResultFileIsInfrastructureRetryableThroughStageDispatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper process wrapper uses a POSIX shell")
	}
	root := initDemo(t)
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunConditions.ClaimsLockTimeout = "20ms"
	if err := instance.WriteConfig(l.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}

	testBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "goobers")
	script := "#!/bin/sh\nexec \"$GOOBERS_TEST_BINARY\" -test.run=^TestClaimLockStageHelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := journal.DefaultScrubber()
	injector, err := credentials.NewInjector(resolver, nil, registry)
	if err != nil {
		t.Fatal(err)
	}
	shell, err := executor.NewShellExecutor(injector, claimLockTestRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	shell.InstanceRoot = root
	shell.SelfBin = wrapper
	dispatch, err := executor.NewTaskExecutor(shell, nil)
	if err != nil {
		t.Fatal(err)
	}

	env := apiv1.InvocationEnvelope{
		TaskID:     "release-claim",
		WorkflowID: "backlog-curation",
		RunID:      "run-stage-lock-timeout",
		Gaggle:     "example",
		Workspace:  t.TempDir(),
	}
	run := apiv1.DeterministicRun{
		Command: []string{"goobers", "backlog-query", "--release"},
		Env: map[string]string{
			"GOOBERS_CLAIM_LOCK_HELPER_PROCESS": "1",
			"GOOBERS_TEST_BINARY":               testBinary,
		},
	}

	lockPath := filepath.Join(l.SchedulerDir(), claimLockFileName)
	holder, err := lock.Acquire(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	started := time.Now()
	result, err := dispatch.Run(context.Background(), env, run)
	if !invoke.IsInfrastructureFailure(err) {
		t.Fatalf("stage result=%+v error=%v, want infrastructure failure", result, err)
	}
	if !strings.Contains(err.Error(), claimsLockTimeoutCode) {
		t.Fatalf("stage error = %q, want typed code %q", err, claimsLockTimeoutCode)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("stage dispatch took %s, want bounded near 20ms", elapsed)
	}

	if err := holder.Release(); err != nil {
		t.Fatal(err)
	}
	result, err = dispatch.Run(context.Background(), env, run)
	if err != nil {
		t.Fatalf("later stage dispatch: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("later stage status = %q, want success: %+v", result.Status, result)
	}
}

func TestClaimLockStageHelperProcess(t *testing.T) {
	if os.Getenv("GOOBERS_CLAIM_LOCK_HELPER_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg != "--" {
			continue
		}
		if i+1 >= len(os.Args) || os.Args[i+1] != "backlog-query" {
			os.Exit(2)
		}
		os.Exit(runBacklogQuery(os.Args[i+2:], os.Stdout, os.Stderr))
	}
	os.Exit(2)
}

type claimLockTestRecorder struct{}

func (claimLockTestRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	return journal.Ref{Path: name, Digest: journal.Digest(data), Size: int64(len(data))}, nil
}

func readSingleSlowClaimLockEvent(t *testing.T, schedulerDir, operation string) journal.Event {
	t.Helper()
	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d instance events, want one: %+v", len(events), events)
	}
	event := events[0]
	if event.Type != journal.EventClaimLockSlow {
		t.Fatalf("event type = %q, want %q", event.Type, journal.EventClaimLockSlow)
	}
	if got := event.Runner["operation"]; got != operation {
		t.Fatalf("operation = %v, want %q", got, operation)
	}
	if got := event.Runner["pid"]; got != float64(os.Getpid()) {
		t.Fatalf("pid = %v, want %d", got, os.Getpid())
	}
	return event
}

func claimLockEventDuration(t *testing.T, event journal.Event, field string) time.Duration {
	t.Helper()
	value, ok := event.Runner[field].(string)
	if !ok {
		t.Fatalf("%s = %#v, want duration string", field, event.Runner[field])
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		t.Fatalf("parse %s %q: %v", field, value, err)
	}
	return duration
}
