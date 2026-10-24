package journalpg

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// SchedulerLog adapts one OpenInstance stream incarnation to the scheduler's
// journal.InstanceStore seam. Its lifetime is the leader epoch: every write
// runs on the epoch context and is lease-fenced by the backend, so a deposed
// leader's appends fail rather than interleave with its successor's.
type SchedulerLog struct {
	backend *Backend
	ctx     context.Context
	handle  journal.InstanceHandle
	token   journal.OwnerToken
	report  func(error)

	closeOnce sync.Once
	closeErr  error
}

// readPage bounds one ReadAfterSeq round trip; the loop pages until drained.
const readPage = 1000

// OpenSchedulerLog acquires the scheduler stream for the epoch bound by ctx.
// report receives best-effort append failures; nil drops them.
func (b *Backend) OpenSchedulerLog(ctx context.Context, report func(error)) (*SchedulerLog, error) {
	handle, token, _, err := b.OpenInstance(ctx)
	if err != nil {
		return nil, err
	}
	return &SchedulerLog{backend: b, ctx: ctx, handle: handle, token: token, report: report}, nil
}

// Append commits one event under the epoch fence.
func (l *SchedulerLog) Append(event journal.Event) error {
	return l.backend.InstanceAppend(l.ctx, l.handle, l.token, event)
}

// AppendBestEffort appends and reports, but never surfaces, a failure.
func (l *SchedulerLog) AppendBestEffort(event journal.Event) {
	if err := l.Append(event); err != nil && l.report != nil {
		l.report(err)
	}
}

// ReadAfterSeq returns every committed event after seq, paging the store.
func (l *SchedulerLog) ReadAfterSeq(seq uint64) ([]journal.Event, error) {
	var out []journal.Event
	for {
		page, err := l.backend.InstanceRead(l.ctx, seq, readPage)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < readPage {
			return out, nil
		}
		seq = page[len(page)-1].Seq
	}
}

// LastSeq returns the stream's committed watermark.
func (l *SchedulerLog) LastSeq() (uint64, error) { return l.backend.InstanceLastSeq(l.ctx) }

// Dir is empty: the cloud scheduler keeps no scheduler directory. Its
// SchedulerState (claims.Store) ignores the argument, and the rate-limit floor
// comes from RateReset rather than a file under Dir.
func (l *SchedulerLog) Dir() string { return "" }

// ErrRateResetUnsupported is the explicit refusal for an operator rate-limit
// reset against the cloud scheduler. It must stay loud: a silent no-op would
// look like a successful reset while MaxRunsPerHour keeps refusing starts.
var ErrRateResetUnsupported = errors.New("journalpg: reset-rate-limit is not supported in cloud yet (tracked with L1d/L1e)")

// ResetRateLimit refuses. The floor is a local file marker today; the cloud
// scheduler has no durable equivalent, so no caller may believe it was applied.
func (l *SchedulerLog) ResetRateLimit(time.Time) error { return ErrRateResetUnsupported }

// RateReset reports no operator reset floor. The cloud has no
// `goobers reset-rate-limit` marker yet, and the scheduler must not fall back
// to reading a relative "rate-reset" file from the process working directory.
func (l *SchedulerLog) RateReset() (time.Time, bool, error) { return time.Time{}, false, nil }

// Close retires the stream incarnation so a successor can reopen it. It is
// idempotent. A retire that loses its lease fails with the fence error, which
// is the correct outcome for a deposed leader: the successor takes the stream
// over by epoch instead.
func (l *SchedulerLog) Close() error {
	l.closeOnce.Do(func() {
		// The epoch context is usually already cancelled when a deposed
		// leader closes; retire must still be attempted under a fresh bound.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), 5*time.Second)
		defer cancel()
		l.closeErr = l.backend.RetireInstance(ctx, l.handle, l.token)
		if errors.Is(l.closeErr, journal.ErrRunOwned) {
			l.closeErr = nil
		}
	})
	return l.closeErr
}

var _ journal.InstanceStore = (*SchedulerLog)(nil)
