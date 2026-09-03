package main

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/hostedprogress"
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
	terminal       bool
	publish        func(context.Context, []journal.Event) error
	publishContext context.Context
	publishFailed  bool
	finalize       func(context.Context, error) error
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

// newHostedRunWaitReporter returns a reporter that additionally publishes the
// versioned hosted-progress contract to one GitHub Check Run whenever the
// journal advances. If --github-progress is not enabled on ctx, the returned
// reporter behaves identically to newRunWaitReporter; if the GitHub Actions
// contract is missing, the failure is surfaced as a one-time warning and the
// reporter continues without publishing (the run itself is not affected).
func newHostedRunWaitReporter(
	ctx context.Context,
	runID, runDir string,
	out io.Writer,
) *runWaitReporter {
	reporter := newRunWaitReporter(runID, out)
	if !githubProgressEnabled(ctx) {
		return reporter
	}
	env, err := hostedprogress.Environment()
	if err != nil {
		reporter.publishFailed = true
		pf(out, "warning: GitHub progress disabled: %v\n", err)
		return reporter
	}
	publisher := hostedprogress.New(env, runDir)
	reporter.publishContext = ctx
	reporter.publish = publisher.Publish
	reporter.finalize = publisher.Finalize
	return reporter
}

// Finalize best-effort completes any in-flight hosted-progress Check Run when
// the caller exits without observing a terminal journal phase (context
// cancellation, timeout, wait error). If hosted progress is disabled or was
// never able to create a Check Run this is a no-op. Errors are surfaced as a
// one-time warning on the reporter's writer so the caller's exit code path
// is unaffected.
func (r *runWaitReporter) Finalize(waitErr error) {
	if r == nil || r.finalize == nil {
		return
	}
	// Cap best-effort finalize latency so a cancelled parent context or a
	// wedged GitHub API does not delay the CLI shutdown path indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.finalize(ctx, waitErr); err != nil && !r.publishFailed {
		r.publishFailed = true
		pf(r.out, "warning: GitHub progress finalize failed: %v\n", err)
	}
}

func (r *runWaitReporter) observe(events []journal.Event, now time.Time) {
	transitioned := false
	for _, event := range events {
		if event.Seq <= r.lastSeq {
			continue
		}
		r.lastSeq = event.Seq

		switch event.Type {
		case journal.EventRunStarted:
			r.runStarted = event.Time
		case journal.EventRunResumed:
			r.terminal = false
			r.pausedGate = ""
			pf(r.out, "run %s resumed by %s at %s (elapsed=%s)\n",
				r.runID, event.Actor, event.Target, r.runElapsed(event.Time))
			transitioned = true
		case journal.EventRunFinished:
			r.terminal = true
		case journal.EventStageStarted:
			key := stageAttempt{stage: event.Stage, attempt: event.Attempt}
			r.stageStarts[key] = event.Time
			pf(r.out, "stage %s started (run=%s, attempt=%d, elapsed=%s)\n",
				event.Stage, r.runID, event.Attempt, r.runElapsed(event.Time))
			transitioned = true
		case journal.EventStageFinished:
			key := stageAttempt{stage: event.Stage, attempt: event.Attempt}
			elapsed := time.Duration(0)
			if started, ok := r.stageStarts[key]; ok {
				elapsed = event.Time.Sub(started)
				delete(r.stageStarts, key)
			}
			pf(r.out, "stage %s finished (run=%s, attempt=%d, status=%s, elapsed=%s)\n",
				event.Stage, r.runID, event.Attempt, event.Status, conciseElapsed(elapsed))
			transitioned = true
		case journal.EventGatePaused:
			if r.pausedGate != event.Gate {
				pf(r.out, "waiting: run %s paused at gate %s (elapsed=%s)\n",
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

	if r.publish != nil && !r.publishFailed {
		if err := r.publish(r.publishContext, events); err != nil {
			r.publishFailed = true
			pf(r.out, "warning: GitHub progress publishing stopped: %v\n", err)
		}
	}

	if transitioned {
		r.lastTransition = now
		r.lastHeartbeat = now
		return
	}
	if r.terminal {
		return
	}
	r.heartbeat(now)
}

func (r *runWaitReporter) heartbeat(now time.Time) {
	if r.terminal || runWaitHeartbeatInterval <= 0 || now.Sub(r.lastHeartbeat) < runWaitHeartbeatInterval {
		return
	}
	pf(r.out, "waiting: run %s has no new transition (elapsed=%s)\n",
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
