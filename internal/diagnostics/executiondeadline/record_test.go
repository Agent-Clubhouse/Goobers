package executiondeadline

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

type recordingAppender struct{ events []journal.Event }

func (r *recordingAppender) Append(event journal.Event) error {
	r.events = append(r.events, event)
	return nil
}

// boundary begins one bounded execution at the fake clock's current time,
// with the given deadline, the way a harness process or session does.
func boundary(t *testing.T, ctx context.Context, deadline time.Time) {
	t.Helper()
	bounded, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	_, done := invoke.BeginExecution(bounded)
	done()
}

// #5871 review: a completion-repair turn is bounded by whatever time the
// stage has left. If its boundary replaced the first, a claim made 28m into
// a 30m stage would be judged against a 2m window and look early.
func TestWindowKeepsTheFirstBoundaryAcrossRepairTurns(t *testing.T) {
	start := time.Date(2026, 9, 26, 18, 37, 20, 0, time.UTC)
	now := start
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = time.Now })

	ctx, window := Observe(context.Background(), nil, "", 1)
	boundary(t, ctx, start.Add(30*time.Minute))
	now = start.Add(28 * time.Minute) // the repair turn starts with 2m left
	boundary(t, ctx, now.Add(2*time.Minute))

	started, deadline, ok := window.Observed()
	if !ok || !started.Equal(start) || !deadline.Equal(start.Add(30*time.Minute)) {
		t.Fatalf("Observed() = %s, %s, %v; want the first turn's %s to %s", started, deadline, ok, start, start.Add(30*time.Minute))
	}
}

func TestWindowWithoutBoundaryIsUnobserved(t *testing.T) {
	_, window := Observe(context.Background(), nil, "", 1)
	if _, _, ok := window.Observed(); ok {
		t.Fatal("no bounded execution began, yet a window was observed")
	}
	var nilWindow *Window
	if _, _, ok := nilWindow.Observed(); ok {
		t.Fatal("a nil window reported an observation")
	}
}

// Observe journals exactly what WithRecorder always did, and fills the
// window even when nothing can be journaled.
func TestObserveJournalsAndFillsTheWindow(t *testing.T) {
	recorder := &recordingAppender{}
	deadline := time.Now().Add(time.Hour)
	ctx, window := Observe(context.Background(), recorder, "prioritize", 0)
	boundary(t, ctx, deadline)
	if len(recorder.events) != 2 || recorder.events[0].Runner["executionState"] != "active" ||
		recorder.events[1].Runner["executionState"] != "finished" || recorder.events[0].Attempt != 1 ||
		recorder.events[0].Runner["deadline"] != deadline.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("journaled = %+v", recorder.events)
	}
	if _, got, ok := window.Observed(); !ok || !got.Equal(deadline) {
		t.Fatalf("window deadline = %s, %v; want %s", got, ok, deadline)
	}

	ctx, unjournaled := Observe(context.Background(), struct{}{}, "prioritize", 1)
	boundary(t, ctx, deadline)
	if _, _, ok := unjournaled.Observed(); !ok {
		t.Fatal("a recorder that cannot append must still fill the window")
	}
	if WithRecorder(context.Background(), struct{}{}, "prioritize", 1) != context.Background() {
		t.Fatal("WithRecorder must leave the context untouched without a journal")
	}
}
