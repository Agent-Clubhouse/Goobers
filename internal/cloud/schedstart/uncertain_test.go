package schedstart

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/goobers/goobers/internal/localscheduler"
)

// notStartedYet is Temporal before a still-'dispatching' start reaches it: the
// workflow ID is unknown, so Get answers NotFound.
type notStartedYet struct{}

type notFoundRun struct{ client.WorkflowRun }

func (notFoundRun) Get(context.Context, interface{}) error {
	return serviceerror.NewNotFound("workflow execution not found")
}

func (notStartedYet) GetWorkflow(context.Context, string, string) client.WorkflowRun {
	return notFoundRun{}
}

func watchUncertain(t *testing.T, cfg Config, run OpenRun, window time.Duration) int32 {
	t.Helper()
	var released atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var tracker Tracker
	cfg.TerminalBackoff = Backoff{Initial: time.Millisecond, Max: 5 * time.Millisecond}
	cfg.WatchReconciled(ctx, []OpenRun{run}, func(string, string) { released.Add(1) }, &tracker)
	time.Sleep(window)
	n := released.Load()
	cancel()
	_ = tracker.Wait(context.Background())
	return n
}

var uncertainRun = OpenRun{Identity: localscheduler.WorkflowIdentity{Gaggle: "g", Workflow: "w"}, RunID: "dispatching-run", AcceptanceID: "trigger-dispatching-run"}

// reviewer-2's probe, adopted. An uncertain ('dispatching') holder counts at
// Prepare because its start may still happen (the recovery re-drives dispatching
// rows about a minute later). Temporal answering NotFound for it means "not
// started yet", not "closed": releasing the slot lets the successor admit
// another run, and the recovered start then runs over MaxConcurrentRuns.
func TestUncertainHolderIsNotReleasedOnNotFoundWhileTheAcceptanceIsLive(t *testing.T) {
	var settledCalls atomic.Int32
	cfg := Config{
		Terminal: TemporalTerminal(notStartedYet{}),
		Settled: func(context.Context, string) (bool, error) {
			settledCalls.Add(1)
			return false, nil // still 'dispatching'
		},
	}
	if n := watchUncertain(t, cfg, uncertainRun, 300*time.Millisecond); n != 0 {
		t.Fatalf("an uncertain holder whose workflow the engine has not seen yet was released %d time(s)", n)
	}
	if settledCalls.Load() == 0 {
		t.Fatal("the acceptance was never re-read after NotFound")
	}
}

// Without a Settled reader the holder stays held: fail closed.
func TestUncertainHolderWithoutASettledReaderStaysHeld(t *testing.T) {
	cfg := Config{Terminal: TemporalTerminal(notStartedYet{})}
	if n := watchUncertain(t, cfg, uncertainRun, 200*time.Millisecond); n != 0 {
		t.Fatalf("released %d time(s) with no way to learn the acceptance ended", n)
	}
}

// Once the acceptance ended without a run (rejected or requeued, or pruned), the
// slot is released; a failing read holds it.
func TestUncertainHolderIsReleasedOnceItsAcceptanceSettled(t *testing.T) {
	var settled atomic.Bool
	var fail atomic.Bool
	cfg := Config{
		Terminal: TemporalTerminal(notStartedYet{}),
		Settled: func(context.Context, string) (bool, error) {
			if fail.Load() {
				return false, errors.New("database unavailable")
			}
			return settled.Load(), nil
		},
	}
	fail.Store(true)
	if n := watchUncertain(t, cfg, uncertainRun, 150*time.Millisecond); n != 0 {
		t.Fatalf("released %d time(s) while the acceptance read was failing", n)
	}
	fail.Store(false)
	settled.Store(true)
	if n := watchUncertain(t, cfg, uncertainRun, 300*time.Millisecond); n != 1 {
		t.Fatalf("released %d time(s) after the acceptance settled, want exactly 1", n)
	}
}

// A holder with no AcceptanceID is a confirmed-started run (a pin): NotFound
// there is permanently gone, and is released.
func TestPinnedHolderIsStillReleasedOnNotFound(t *testing.T) {
	run := uncertainRun
	run.AcceptanceID = ""
	cfg := Config{Terminal: TemporalTerminal(notStartedYet{})}
	if n := watchUncertain(t, cfg, run, 300*time.Millisecond); n != 1 {
		t.Fatalf("released %d time(s), want 1 for a confirmed run the engine no longer knows", n)
	}
}
