package main

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

var runWaitHeartbeatInterval = 30 * time.Second

type synchronizedWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

type stageAttempt struct {
	stage   string
	attempt int
}

type runWaitReporter struct {
	runID          string
	out            io.Writer
	waitStarted    time.Time
	runStarted     time.Time
	lastSeq        uint64
	lastTransition time.Time
	lastHeartbeat  time.Time
	stageStarts    map[stageAttempt]time.Time
	pausedGate     string
}

func newRunWaitReporter(runID string, out io.Writer) *runWaitReporter {
	if out == nil {
		out = io.Discard
	}
	now := time.Now()
	return &runWaitReporter{
		runID:          runID,
		out:            out,
		waitStarted:    now,
		lastTransition: now,
		lastHeartbeat:  now,
		stageStarts:    make(map[stageAttempt]time.Time),
	}
}

func (r *runWaitReporter) observe(events []journal.Event, now time.Time) {
	transitioned := false
	terminal := false
	for _, event := range events {
		if event.Seq <= r.lastSeq {
			continue
		}
		r.lastSeq = event.Seq

		switch event.Type {
		case journal.EventRunStarted:
			r.runStarted = event.Time
		case journal.EventRunFinished:
			terminal = true
		case journal.EventStageStarted:
			key := stageAttempt{stage: event.Stage, attempt: event.Attempt}
			r.stageStarts[key] = event.Time
			fmt.Fprintf(r.out, "stage %s started (run=%s, attempt=%d, elapsed=%s)\n",
				event.Stage, r.runID, event.Attempt, r.runElapsed(event.Time))
			transitioned = true
		case journal.EventStageFinished:
			key := stageAttempt{stage: event.Stage, attempt: event.Attempt}
			elapsed := time.Duration(0)
			if started, ok := r.stageStarts[key]; ok {
				elapsed = event.Time.Sub(started)
				delete(r.stageStarts, key)
			}
			fmt.Fprintf(r.out, "stage %s finished (run=%s, attempt=%d, status=%s, elapsed=%s)\n",
				event.Stage, r.runID, event.Attempt, event.Status, conciseElapsed(elapsed))
			transitioned = true
		case journal.EventGatePaused:
			if r.pausedGate != event.Gate {
				fmt.Fprintf(r.out, "waiting: run %s paused at gate %s (elapsed=%s)\n",
					r.runID, event.Gate, r.runElapsed(event.Time))
				r.pausedGate = event.Gate
				transitioned = true
			}
		case journal.EventGateEvaluated:
			if r.pausedGate == event.Gate {
				r.pausedGate = ""
			}
		}
	}

	if transitioned {
		r.lastTransition = now
		r.lastHeartbeat = now
		return
	}
	if terminal {
		return
	}
	r.heartbeat(now)
}

func (r *runWaitReporter) heartbeat(now time.Time) {
	if runWaitHeartbeatInterval <= 0 || now.Sub(r.lastHeartbeat) < runWaitHeartbeatInterval {
		return
	}
	fmt.Fprintf(r.out, "waiting: run %s has no new transition (elapsed=%s)\n",
		r.runID, conciseElapsed(now.Sub(r.lastTransition)))
	r.lastHeartbeat = now
}

func (r *runWaitReporter) runElapsed(at time.Time) time.Duration {
	started := r.runStarted
	if started.IsZero() {
		started = r.waitStarted
	}
	return conciseElapsed(at.Sub(started))
}

func conciseElapsed(elapsed time.Duration) time.Duration {
	if elapsed < 0 {
		return 0
	}
	return elapsed.Round(time.Second)
}
