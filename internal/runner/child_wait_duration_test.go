package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/hostsuspend"
	"github.com/goobers/goobers/internal/journal"
)

func recordedChildWait(t *testing.T) (*Runner, *journal.Run, taskFrame, []journal.Event, journal.Event) {
	t.Helper()
	r, run, frame := childOriginRuntime(t, &childOriginGoober{})
	if err := frame.recordTaskStarted(1, ""); err != nil {
		t.Fatal(err)
	}
	record := childWaitRecord{Version: 1, ParentRunID: frame.in.RunID, Request: ChildHandoffRequest{Gaggle: "web", ParentRunID: frame.in.RunID, RequestID: journal.Digest([]byte("wait")), Action: "wait", ChildRunID: "child-run", AcceptanceID: "trigger-child-run", InvocationKey: "child", SourceDigest: journal.Digest([]byte("source")), Origin: *frame.childOrigin}}
	event, err := childWaitEvent(frame.t.Name, 1, "", record)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(event); err != nil {
		t.Fatal(err)
	}
	reader, _ := journal.OpenReadOnly(run.Dir())
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	return r, run, frame, events, events[len(events)-1]
}

func TestRunExecutionElapsedExcludesChildWaitAndStopsAtSettlement(t *testing.T) {
	_, _, _, events, marker := recordedChildWait(t)
	start := marker.Time.Add(-time.Minute)
	now := marker.Time.Add(30 * 24 * time.Hour)
	elapsed, err := RunExecutionElapsed(events, start, now)
	if err != nil || elapsed != time.Minute {
		t.Fatalf("parked execution elapsed = %s, %v", elapsed, err)
	}
	request, parked, err := ParkedChildRequest(events)
	if err != nil || !parked {
		t.Fatal(request, parked, err)
	}
	for _, terminal := range []journal.Event{
		{Type: journal.EventRunnerAnnotation, Stage: marker.Stage, Attempt: marker.Attempt, Runner: map[string]any{"kind": ChildContinuedKind, "requestId": request.RequestID}},
		{Type: journal.EventStageFinished, Stage: marker.Stage, Attempt: marker.Attempt},
		{Type: journal.EventRunFinished},
	} {
		terminal.Time = now.Add(-2 * time.Minute)
		elapsed, err := RunExecutionElapsed(append(events, terminal), start, now)
		if err != nil || elapsed != 3*time.Minute {
			t.Fatalf("%s elapsed = %s, %v", terminal.Type, elapsed, err)
		}
	}
	if _, err := RunExecutionElapsed(events, start, marker.Time.Add(-time.Second)); err == nil {
		t.Fatal("future child custody accepted")
	}
	if _, err := RunExecutionElapsed(events, marker.Time.Add(time.Second), now); err == nil {
		t.Fatal("wait preceding run lifetime accepted")
	}
	if elapsed, err := RunExecutionElapsed(nil, now.Add(time.Second), now); err != nil || elapsed != -time.Second {
		t.Fatal("ordinary clock rollback behavior changed", elapsed, err)
	}
}

// #5891: maxRunDuration counts active execution, so host suspension is
// excluded, and a suspension that overlaps a child wait is excluded only once.
func TestRunExecutionElapsedExcludesHostSuspension(t *testing.T) {
	start := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	now := start.Add(24 * time.Hour)
	suspended := []hostsuspend.Window{
		{From: start.Add(9 * time.Hour), To: start.Add(13 * time.Hour)},
		{From: start.Add(17 * time.Hour), To: start.Add(22 * time.Hour)},
	}
	elapsed, err := RunExecutionElapsed(nil, start, now, suspended...)
	if err != nil || elapsed != 15*time.Hour {
		t.Fatalf("suspended execution elapsed = %s, %v; want 15h", elapsed, err)
	}

	_, _, _, events, marker := recordedChildWait(t)
	start = marker.Time.Add(-time.Minute)
	now = marker.Time.Add(30 * 24 * time.Hour)
	overlapping := hostsuspend.Window{From: marker.Time.Add(-30 * time.Second), To: marker.Time.Add(time.Hour)}
	elapsed, err = RunExecutionElapsed(events, start, now, overlapping)
	if err != nil || elapsed != 30*time.Second {
		t.Fatalf("child wait with overlapping suspension elapsed = %s, %v; want 30s", elapsed, err)
	}
}

func TestExpireRunUsesChildExecutionClock(t *testing.T) {
	r, _, frame, _, marker := recordedChildWait(t)
	start := marker.Time.Add(-time.Minute)
	result, expired, err := r.ExpireRun(frame.in.RunID, marker.Time.Add(30*24*time.Hour), start, time.Hour)
	if err != nil || expired || result.Phase != journal.PhaseRunning {
		t.Fatal(result, expired, err)
	}
}

type childCapacityResult struct{ resume func(context.Context) error }

func (s childCapacityResult) Resume(ctx context.Context) error { return s.resume(ctx) }

func TestChildCapacityWaitSurfacesPolicyAndUnknownOwnership(t *testing.T) {
	_, run, frame := childOriginRuntime(t, &childOriginGoober{})
	ctx, cancel := context.WithCancel(t.Context())
	suspension := childCapacityResult{resume: func(context.Context) error {
		cancel()
		return &ChildCapacityWaitError{Reason: "workflow_disabled", PolicyBlocked: true}
	}}
	if err := resumeChildCapacity(ctx, suspension, &frame, 1, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reader, _ := journal.OpenReadOnly(run.Dir())
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Runner["kind"] != "child.workflow.capacity-blocked" || last.Runner["reason"] != "workflow_disabled" || last.Runner["policyBlocked"] != true {
		t.Fatal("missing actionable policy block", last)
	}
	ownership := errors.New("owner no longer exists")
	calls := 0
	suspension.resume = func(context.Context) error { calls++; return ownership }
	if err := resumeChildCapacity(t.Context(), suspension, &frame, 1, ""); !errors.Is(err, ownership) || calls != 1 {
		t.Fatal("unknown ownership silently retried", err, calls)
	}
}
