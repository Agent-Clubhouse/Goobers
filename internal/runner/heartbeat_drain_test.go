package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// recordingHeartbeatJournal captures the events a heartbeat emits.
type recordingHeartbeatJournal struct {
	mu     sync.Mutex
	events []journal.Event
}

func (j *recordingHeartbeatJournal) Append(e journal.Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, e)
	return nil
}
func (j *recordingHeartbeatJournal) ObserveActivity()            {}
func (j *recordingHeartbeatJournal) RepairAppendBoundary() error { return nil }

func (j *recordingHeartbeatJournal) heartbeats() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := 0
	for _, e := range j.events {
		if e.Type == journal.EventStageHeartbeat {
			n++
		}
	}
	return n
}

// #3455: a graceful drain cancels the stage context and then waits up to the
// full termination grace period for the stage to finish. If the heartbeat
// stopped at cancellation, the journal would go silent while the stage was
// still healthy and working -- and the drain's own progress line tells the
// operator to send another signal, which kills exactly the stage the grace
// period exists to protect. Observed live: seven minutes of silence on a
// 60-second cadence while `go test -race` burned two cores.
//
// So cancellation must NOT end the heartbeat. Only the stage actually
// finishing (Stop) may.
func TestStageHeartbeatSurvivesContextCancellation(t *testing.T) {
	ticker := &fakeHeartbeatTicker{
		ticks:   make(chan time.Time),
		stopped: make(chan struct{}),
	}
	r := &Runner{
		heartbeatInterval:  StageHeartbeatInterval,
		newHeartbeatTicker: func(time.Duration) heartbeatTicker { return ticker },
	}
	jr := &recordingHeartbeatJournal{}

	ctx, cancel := context.WithCancel(context.Background())
	stageCtx, heartbeat := r.startStageHeartbeat(ctx, jr, "local-ci", 1, journal.AttemptPolicy)

	// The drain arrives: context cancelled, stage still running.
	cancel()

	// The stage keeps making progress and the ticker keeps firing. Each
	// heartbeat needs a progress report since the previous tick, which is
	// exactly what a live stage produces.
	//
	// Waiting for each heartbeat to land before producing the next one keeps
	// this deterministic: the emitter consumes the progress flag with a Swap,
	// so reporting progress for the next tick before the previous tick has
	// been consumed would let one tick observe a false flag and skip -- a race
	// in the test, not in the code under test.
	for i := 1; i <= 3; i++ {
		invoke.ReportProgress(stageCtx)
		// Send with a deadline rather than bare. If the emitter regresses to
		// exiting on cancellation, nothing receives this tick and a bare send
		// would hang the whole package until the test binary times out --
		// turning a clear regression into an unreadable stall.
		select {
		case ticker.ticks <- time.Now():
		case <-time.After(5 * time.Second):
			t.Fatalf("tick %d had no receiver -- the heartbeat emitter exited early, most likely on context cancellation (#3455)", i)
		}
		deadline := time.Now().Add(2 * time.Second)
		for jr.heartbeats() < i {
			if time.Now().After(deadline) {
				t.Fatalf("heartbeat %d never landed (have %d) -- a cancelled context silenced a running stage", i, jr.heartbeats())
			}
			time.Sleep(time.Millisecond)
		}
	}

	// Stop is what finishTaskDispatch calls when the stage genuinely ends.
	if err := heartbeat.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := jr.heartbeats(); got < 3 {
		t.Fatalf("heartbeats after cancellation = %d, want at least 3: a cancelled context must not silence a running stage", got)
	}
}

// The complement: once the stage really ends, the heartbeat must stop. Without
// this, removing the cancellation case would leak a goroutine per stage.
func TestStageHeartbeatStopsWhenStageEnds(t *testing.T) {
	ticker := &fakeHeartbeatTicker{
		ticks:   make(chan time.Time),
		stopped: make(chan struct{}),
	}
	r := &Runner{
		heartbeatInterval:  StageHeartbeatInterval,
		newHeartbeatTicker: func(time.Duration) heartbeatTicker { return ticker },
	}
	jr := &recordingHeartbeatJournal{}

	_, heartbeat := r.startStageHeartbeat(context.Background(), jr, "local-ci", 1, journal.AttemptPolicy)
	if err := heartbeat.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-ticker.stopped:
	default:
		t.Fatal("ticker was not stopped when the stage ended")
	}
}
