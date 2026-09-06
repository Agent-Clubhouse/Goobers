package harness

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// activity.go makes a stalled agentic session visible from the run's own
// journal (#4179).
//
// MEASURED: run f5faeec4ee947f88af7a09204db51bcb reached `implement` on PR
// #4167 and burned the whole 5400s budget. The journal held ONE
// agent.lifecycle event and nothing else for ninety minutes, so "the agent is
// looping" and "the agent is blocked on a Go module download that will never
// return" were indistinguishable from the run's record — and the obvious
// reflex was to raise the budget a third time. The transcript artifact showed
// the truth (a productive session, then `go mod download all` hanging), but
// only to someone who knew the artifact existed and went to read it by hand.
//
// The distinguishing evidence is cheap and already flowing through this
// package: a looping session keeps producing output, a blocked one goes
// silent. What was missing was anything that WROTE that distinction down.

// DefaultActivityInterval is how often a running subprocess is sampled for
// output. Long enough that a 90-minute session adds a bounded handful of
// journal events rather than a stream, short enough that a stall is named
// within a few minutes of starting rather than after the budget expires.
const DefaultActivityInterval = 5 * time.Minute

// ActivitySample is one observation of a running subprocess's output.
type ActivitySample struct {
	// Elapsed is time since the process started.
	Elapsed time.Duration
	// Idle is time since the last byte of output was observed. Equal to
	// Elapsed when the process has produced nothing at all.
	Idle time.Duration
	// Bytes is total output observed so far, retained plus dropped — so a
	// session that has passed the transcript cap still reads as producing.
	Bytes int64
	// Moved reports whether any output arrived during this interval. This is
	// the whole point: it separates a working session from a stalled one.
	Moved bool
}

// ActivityObserver receives one ActivitySample per interval while a
// subprocess runs. It is called from a sampler goroutine, never on the path
// that copies the subprocess's output, so a slow observer delays the next
// sample but can never block the process it is describing. It must not panic.
type ActivityObserver func(ActivitySample)

// activityTracker records output arrivals cheaply enough to sit on the
// per-write path: one atomic store per Write call, no allocation, no lock
// ordering against the transcript buffer's own mutex.
type activityTracker struct {
	startedAt time.Time
	// lastOutputNanos is the monotonic-ish offset from startedAt of the most
	// recent write, so idle time needs no wall-clock read on the hot path.
	lastOutputNanos atomic.Int64
	// writes counts Write calls, used only to detect "did anything arrive
	// since the last sample" without reading the buffer's guarded length.
	writes atomic.Int64
}

func newActivityTracker(startedAt time.Time) *activityTracker {
	return &activityTracker{startedAt: startedAt}
}

// observe is the hot-path hook: called for every non-empty write to the
// transcript buffer.
func (t *activityTracker) observe(now time.Time) {
	t.lastOutputNanos.Store(int64(now.Sub(t.startedAt)))
	t.writes.Add(1)
}

// sample builds one ActivitySample and reports whether output arrived since
// the previous call. prevWrites is the writes count at the previous sample.
func (t *activityTracker) sample(now time.Time, prevWrites int64, bytes int64) (ActivitySample, int64) {
	writes := t.writes.Load()
	elapsed := now.Sub(t.startedAt)
	idle := elapsed
	if last := t.lastOutputNanos.Load(); last > 0 {
		idle = elapsed - time.Duration(last)
	}
	if idle < 0 {
		idle = 0
	}
	return ActivitySample{
		Elapsed: elapsed,
		Idle:    idle,
		Bytes:   bytes,
		Moved:   writes > prevWrites,
	}, writes
}

// activitySampler drives an ActivityObserver on a ticker for the lifetime of
// one subprocess. stop is idempotent and WAITS for the sampler goroutine, so
// no sample can land after the caller has moved on to reporting the process's
// terminal state — an out-of-order "still waiting" event after a "completed"
// one would be worse than no event at all.
type activitySampler struct {
	stopOnce sync.Once
	done     chan struct{}
	finished chan struct{}
}

func startActivitySampler(ctx context.Context, tracker *activityTracker, interval time.Duration, bytes func() int64, observe ActivityObserver) *activitySampler {
	if observe == nil || tracker == nil {
		return nil
	}
	if interval <= 0 {
		interval = DefaultActivityInterval
	}
	s := &activitySampler{done: make(chan struct{}), finished: make(chan struct{})}
	go func() {
		defer close(s.finished)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var prevWrites int64
		for {
			select {
			case <-s.done:
				return
			case <-ctx.Done():
				// The process is being killed (timeout or cancel). Its
				// terminal event says so; another liveness mark here would
				// only add noise to the moment that is already legible.
				return
			case now := <-ticker.C:
				var observed int64
				if bytes != nil {
					observed = bytes()
				}
				sample, writes := tracker.sample(now, prevWrites, observed)
				prevWrites = writes
				observe(sample)
			}
		}
	}()
	return s
}

// stop halts sampling and waits for the goroutine to exit. Safe on a nil
// sampler, which is what startActivitySampler returns when no observer is
// wired, so callers can defer it unconditionally.
func (s *activitySampler) stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.done) })
	<-s.finished
}
