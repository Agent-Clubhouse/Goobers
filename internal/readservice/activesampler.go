package readservice

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

// The active-run sampler (#1741, design §17 Wave 1.4).
//
// # What it removes
//
// Six production call sites answered "how many runs are live" by walking every
// run directory in history and replaying each run's event log to reconstruct its
// phase. Measured on the live instance: 17.2 s cold / 4.3 s warm to return the
// answer "2", against a client that aborts at 10 s (§2.1). Measured in the Wave 0
// harness at 1x: 4.06 s p50, 11.26 s p99, walking 40,665 directories and opening
// 29,759 journals — per request.
//
// The read paths were paying that. Now a background sampler pays it, once per
// interval, and readers take the last sample from memory with its age.
//
// # Why "no sample" must not fall back to the scan
//
// §17 Wave 1.4 corrects an earlier version of the design that "kept the
// 17-second scan as the no-sample fallback and so preserved the exact failure on
// a cold daemon". That is the whole trap: a fallback which is only taken when
// the cache is cold is taken exactly when the instance is busiest — at startup,
// after a restart, during a deploy — so the mitigation evaporates under the
// conditions that motivated it.
//
// So a reader with no sample gets a typed "not sampled yet" and the caller
// renders that state. It is the §7.2 rule applied early: current, stale by a
// stated amount, or unavailable with a reason — never an unbounded wait.
//
// # Deliberately throwaway
//
// Projected topologies now sample an indexed query over the stored phase column.
// The historical walk remains for topologies without a projection.

// ErrActiveCountsUnavailable reports that no active-run sample has been taken
// yet. Callers surface it as an explicitly degraded state rather than blocking
// or falling back to the synchronous walk.
var ErrActiveCountsUnavailable = errors.New("readservice: active run counts not sampled yet")

// ErrActiveSamplerStopTimeout reports that sampler shutdown reached its finite
// wait bound because an in-flight filesystem operation did not return.
var ErrActiveSamplerStopTimeout = errors.New("readservice: active run sampler shutdown timed out")

// activeSample is one completed observation of the active-run set.
type activeSample struct {
	counts  map[localscheduler.WorkflowIdentity]int
	takenAt time.Time
	// err is the sampling error, retained so a persistently failing sample is
	// reported rather than silently serving an ever-staler success. One
	// unreadable event log fails the whole walk today (localscheduler returns
	// the error), so this is reachable.
	err error
}

// activeRunSampler samples active counts on an interval and publishes the result.
//
// It holds no reference to a request context: the walk must not be cancellable
// by whichever reader happened to trigger it, which was one of the ways the old
// request-path scan wasted work — a client giving up at 10 s discarded a scan
// that was 9 s in.
type activeRunSampler struct {
	layout   instance.Layout
	interval time.Duration
	now      func() time.Time
	walk     func(context.Context) (map[localscheduler.WorkflowIdentity]int, error)

	stopTimeout time.Duration

	mu     sync.RWMutex
	sample *activeSample

	// sampling guards against overlapping samples. A historical walk takes seconds; an
	// interval shorter than the walk would otherwise stack them and turn a
	// mitigation into a load generator.
	sampling sync.Mutex

	lifecycleMu sync.Mutex
	started     bool
	stopped     bool
	stop        chan struct{}
	done        chan struct{}
	doneOnce    sync.Once
	cancel      context.CancelFunc
}

// defaultActiveSampleInterval is how often the sampler refreshes.
//
// Chosen against the measured walk cost rather than a round number: at 1x the
// walk is ~4 s, so 30 s keeps duty cycle near 13% while bounding staleness to
// well under the time a human spends reading a page. The age is reported either
// way, so a stale sample is visible rather than assumed fresh.
const defaultActiveSampleInterval = 30 * time.Second

// activeSamplerStopTimeout bounds daemon shutdown when an operating-system
// filesystem call cannot be interrupted by context cancellation.
const activeSamplerStopTimeout = 5 * time.Second

// newActiveRunSampler creates a sampler. It does not start until Start is
// called, so a read service constructed for a one-shot CLI command does not
// spawn a goroutine it will never use.
func newActiveRunSampler(layout instance.Layout, interval time.Duration, now func() time.Time) *activeRunSampler {
	if interval <= 0 {
		interval = defaultActiveSampleInterval
	}
	if now == nil {
		now = time.Now
	}
	sampler := &activeRunSampler{
		layout:      layout,
		interval:    interval,
		now:         now,
		stopTimeout: activeSamplerStopTimeout,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	sampler.walk = sampler.walkRuns
	return sampler
}

// Start begins sampling in the background. Repeated calls are idempotent. The
// first sample is taken immediately, so a daemon that has been up for one
// interval is not still reporting "unavailable".
func (a *activeRunSampler) Start() {
	a.lifecycleMu.Lock()
	if a.started || a.stopped {
		a.lifecycleMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.started = true
	a.lifecycleMu.Unlock()

	go func() {
		defer a.doneOnce.Do(func() { close(a.done) })
		a.refresh(ctx)
		ticker := time.NewTicker(a.interval)
		defer ticker.Stop()
		for {
			select {
			case <-a.stop:
				return
			case <-ticker.C:
				a.refresh(ctx)
			}
		}
	}()
}

// Stop cancels sampling and waits up to five seconds for the in-flight walk.
// It returns ErrActiveSamplerStopTimeout if lower-level I/O cannot be
// interrupted within that bound. Repeated calls are safe.
func (a *activeRunSampler) Stop() error {
	a.lifecycleMu.Lock()
	if !a.stopped {
		a.stopped = true
		close(a.stop)
		if a.cancel != nil {
			a.cancel()
		}
	}
	if !a.started {
		a.doneOnce.Do(func() { close(a.done) })
	}
	a.lifecycleMu.Unlock()

	timer := time.NewTimer(a.stopTimeout)
	defer timer.Stop()
	select {
	case <-a.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("%w after %s", ErrActiveSamplerStopTimeout, a.stopTimeout)
	}
}

// refresh takes one sample.
func (a *activeRunSampler) refresh(ctx context.Context) {
	// Skip rather than queue if a sample is already running: overlapping
	// multi-second walks would compound rather than refresh.
	if !a.sampling.TryLock() {
		return
	}
	defer a.sampling.Unlock()

	counts, err := a.walk(ctx)
	a.mu.Lock()
	a.sample = &activeSample{counts: counts, takenAt: a.now(), err: err}
	a.mu.Unlock()
}

// walk performs the O(history) directory walk the read paths no longer do.
func (a *activeRunSampler) walkRuns(ctx context.Context) (map[localscheduler.WorkflowIdentity]int, error) {
	runDirs, err := a.layout.RunDirsContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate run roots: %w", err)
	}
	counts, err := localscheduler.ActiveRunCountsByWorkflowDirsContext(ctx, runDirs)
	if err != nil {
		return nil, fmt.Errorf("read active run projection: %w", err)
	}
	return counts, nil
}

// Counts returns the most recent sample and how old it is.
//
// It never walks. A caller that gets ErrActiveCountsUnavailable must render the
// degraded state, not retry synchronously — see the package comment for why the
// fallback is the trap rather than the safety net.
func (a *activeRunSampler) Counts() (map[localscheduler.WorkflowIdentity]int, time.Duration, error) {
	a.mu.RLock()
	sample := a.sample
	a.mu.RUnlock()

	if sample == nil {
		return nil, 0, ErrActiveCountsUnavailable
	}
	age := a.now().Sub(sample.takenAt)
	if sample.err != nil {
		return nil, age, sample.err
	}
	return sample.counts, age, nil
}

// SampleNow forces a synchronous sample. It exists for tests and for one-shot
// CLI commands that have no daemon to have warmed the cache and would otherwise
// always report unavailable; it is never called from an HTTP read path.
func (a *activeRunSampler) SampleNow(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.refresh(ctx)
	_, _, err := a.Counts()
	return err
}
