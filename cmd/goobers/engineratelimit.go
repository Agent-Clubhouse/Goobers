package main

import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

// engineRateLimitObserver watches a live journal's appends for a stage that
// failed rate-limited, and records the provider's reset instant in the
// scheduler's quota state.
//
// # Why this is an observer and not a terminal hook
//
// The point of #614's RateLimited handler is that it fires MID-RUN. A stage
// that hits the provider's rate limit tells the scheduler "stop dispatching
// more provider-dependent runs until the window rolls over", and that
// instruction is worthless if it arrives when the run ends: a long agentic run
// can keep executing for tens of minutes after the limit is hit, during which
// the scheduler would keep admitting new runs that are all guaranteed to fail
// the same way. The local runner calls the handler from inside its walk
// (notifyRateLimited, at the failing stage). An engine run's walk is on a
// Temporal worker with no access to this daemon's ProviderQuotaState — but
// every stage result it emits passes through this daemon's live journal
// writer on the way to disk. That is the seam.
//
// # Why it needs the event body
//
// livejournal.WithObserver — journal.WithAppendObserver's shape — delivers
// only (runID, seq). Deciding rate-limitedness from that requires re-opening
// and re-reading the run journal on every single append, on a path that holds
// the run's lock. livejournal.WithEventObserver (added for this) delivers the
// event, so the decision is three map lookups.
//
// # What it keys on
//
// The stage.finished event's own error code and its declared outputs — the
// C5 rate-limit pair. executor.CIPollFailureCode maps a
// *providers.RateLimitError to providers.ErrorCodeRateLimited, and the stage
// writes its reset instant to executor.OutputRateLimitReset ("rateLimitReset")
// in the same result file. Both halves must be present: a rate-limited
// failure whose reset could not be recovered carries nothing actionable, so
// it is skipped exactly as internal/runner's taskOutcome skips it, rather
// than recording a zero reset that would park the scheduler forever.
func engineRateLimitObserver(quota *localscheduler.ProviderQuotaState) func(runID string, ev journal.Event) {
	if quota == nil {
		return nil
	}
	return func(_ string, ev journal.Event) {
		if !rateLimitedStageEvent(ev) {
			return
		}
		resetAt, ok := runner.OutputRateLimitReset(ev.Outputs)
		if !ok {
			// A continueOnError'd failure has its outputs discarded (the
			// local runner discards them too), so the engine carries the
			// reset on the event's Runner map instead. Without this arm a
			// tolerated rate-limited stage would leave the quota state
			// untouched and the scheduler would keep admitting runs into an
			// exhausted window — the exact case #614 exists for.
			resetAt, ok = runner.OutputRateLimitReset(ev.Runner)
		}
		if !ok {
			return
		}
		quota.RecordExhausted(resetAt)
	}
}

// rateLimitedStageEvent reports whether ev is a stage outcome that failed
// with the typed rate-limited code.
//
// Both the stage.finished event and the error event a failing stage may also
// append can carry the code; keying on the code rather than the event type
// keeps the observer correct if a future emission site changes which of the
// two carries the outputs, and costs one comparison.
func rateLimitedStageEvent(ev journal.Event) bool {
	if ev.Error == nil {
		return false
	}
	if ev.Error.Code != providers.ErrorCodeRateLimited {
		return false
	}
	return len(ev.Outputs) > 0 || len(ev.Runner) > 0
}

// engineRateLimitOption builds the writer option, or nil when there is no
// quota state to record into.
func engineRateLimitOption(quota *localscheduler.ProviderQuotaState) livejournal.Option {
	observer := engineRateLimitObserver(quota)
	if observer == nil {
		return nil
	}
	return livejournal.WithEventObserver(observer)
}
