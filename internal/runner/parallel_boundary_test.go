package runner

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestParallelChildRecoveryIsNotANewExecutionBoundary(t *testing.T) {
	r, run, frame := childOriginRuntime(t, &childOriginGoober{})
	par := newParallelExec(apiv1.Parallel{BranchTimeoutSeconds: 1})
	branch := branchState{id: 1, startedAt: time.Now().Add(-time.Hour)}
	steps := &atomic.Int64{}
	var result parallelBranchResult
	if r.stopParallelBranchAtBoundary(t.Context(), run, frame.in, par, branch, steps, &result, &resumeContext{}) || steps.Load() != 0 {
		t.Fatal("child recovery consumed a fresh step or timed out mid-stage", result)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !r.stopParallelBranchAtBoundary(ctx, run, frame.in, par, branch, steps, &result, &resumeContext{}) || !result.paused {
		t.Fatal("recovery ignored shutdown")
	}
	result = parallelBranchResult{}
	if !r.stopParallelBranchAtBoundary(t.Context(), run, frame.in, par, branch, steps, &result, nil) || result.status != journal.BranchTimedOut {
		t.Fatal("the next execution boundary ignored its expired budget", result)
	}
}

func TestParallelChildClockExcludesOnlyCurrentBranchWait(t *testing.T) {
	_, _, frame := childOriginRuntime(t, &childOriginGoober{})
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	started := now
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: frame.in.RunID}, nil, journal.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	appendEvent := func(event journal.Event) {
		t.Helper()
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	parallel := journal.Event{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{{Branch: 1}, {Branch: 2}}}
	appendEvent(parallel)
	appendEvent(journal.Event{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "a", Stage: frame.t.Name})
	appendEvent(journal.Event{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 2, BranchName: "b", Stage: "other"})
	frame.jr = &branchJournal{run: run, branch: 1}
	if err := frame.recordTaskStarted(1, ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	record := childWaitRecord{Version: 1, ParentRunID: frame.in.RunID, Request: ChildHandoffRequest{Gaggle: frame.in.Gaggle, ParentRunID: frame.in.RunID, RequestID: journal.Digest([]byte("wait")), Action: "wait", ChildRunID: "child-run", AcceptanceID: "trigger-child-run", InvocationKey: "child", SourceDigest: journal.Digest([]byte("source")), Origin: *frame.childOrigin}}
	wait, err := childWaitEvent(frame.t.Name, 1, "", record)
	if err != nil {
		t.Fatal(err)
	}
	wait.Branch = 1
	appendEvent(wait)
	check := func(branch int, start time.Time, want bool) {
		t.Helper()
		expired, err := parallelBranchExpired(run, frame.in, "fan", branchState{id: branch, startedAt: start}, 120, now)
		if err != nil || expired != want {
			t.Fatalf("branch %d expired=%v error=%v; want %v", branch, expired, err, want)
		}
	}
	now = now.Add(30 * 24 * time.Hour)
	check(1, started, false)
	check(2, started, true)
	appendEvent(journal.Event{Type: journal.EventRunnerAnnotation, Branch: 1, Stage: frame.t.Name, Attempt: 1, Runner: map[string]any{"kind": ChildContinuedKind, "requestId": record.Request.RequestID}})
	now = now.Add(59 * time.Second)
	check(1, started, false)
	now = now.Add(time.Second)
	check(1, started, true)
	appendEvent(journal.Event{Type: journal.EventStageFinished, Branch: 1, Stage: frame.t.Name, Attempt: 1, Status: string(apiv1.ResultSuccess)})
	appendEvent(journal.Event{Type: journal.EventBranchFinished, Branch: 1, Parallel: "fan", BranchStatus: journal.BranchSucceeded})
	appendEvent(journal.Event{Type: journal.EventBranchFinished, Branch: 2, Parallel: "fan", BranchStatus: journal.BranchSucceeded})
	appendEvent(journal.Event{Type: journal.EventParallelFinished, Parallel: "fan"})
	// A later visit reuses branch 1, but cannot inherit thirty days of credit.
	started = now
	appendEvent(parallel)
	appendEvent(journal.Event{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "a", Stage: frame.t.Name})
	now = now.Add(3 * time.Minute)
	check(1, started, true)
}
