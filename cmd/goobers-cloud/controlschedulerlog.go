package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
)

// schedulerLogOpener opens the scheduler stream of the epoch bound by ctx;
// *journalpg.Backend satisfies it.
type schedulerLogOpener interface {
	OpenSchedulerLog(ctx context.Context, report func(error)) (*journalpg.SchedulerLog, error)
}

// schedulerLogs hands every consumer of one lease epoch the SAME fenced
// scheduler log. Opening the stream takes its owner token, and a second open in
// the same epoch is refused (journal.ErrRunOwned), so the scheduler admission
// and the terminal hooks (which record their outcomes to the instance log) must
// share one incarnation rather than each opening their own.
//
// The log is bound to the context of the first open and reports append failures
// to that open's report func; callers must pass the epoch context and share one
// reporter. A new epoch reopens. A failed open is not remembered, so a later
// call retries.
//
// Lifetime: the OPENER owns Close. It retires the stream exactly once, when the
// epoch context ends; a lost-lease retire is the expected result for a deposed
// leader and is not reported. Every consumer gets a view whose Close does
// nothing, so one consumer stopping (the scheduler admission at the end of its
// epoch) can never break another's later appends (terminal hook outcomes).
type schedulerLogs struct {
	mu    sync.Mutex
	epoch int64
	log   *sharedSchedulerLog
	// retire closes the underlying log; nil closes it directly. Tests count it.
	retire func(*journalpg.SchedulerLog) error
}

// sharedSchedulerLog is a consumer's view of the epoch's scheduler log. It keeps
// every method of the log (the scheduler also reads its rate-reset state) except
// Close, which is the opener's alone.
type sharedSchedulerLog struct{ *journalpg.SchedulerLog }

var _ journal.InstanceStore = (*sharedSchedulerLog)(nil)

// Close does nothing: the opener retires the stream when the epoch ends.
func (*sharedSchedulerLog) Close() error { return nil }

// For returns the epoch's scheduler log, opening it on the first call.
func (s *schedulerLogs) For(ctx context.Context, opener schedulerLogOpener, handle *lease.Handle, report func(error)) (*sharedSchedulerLog, error) {
	if opener == nil || handle == nil {
		return nil, errors.New("scheduler log requires the journal backend and the epoch's lease handle")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log != nil && s.epoch == handle.Epoch {
		return s.log, nil
	}
	log, err := opener.OpenSchedulerLog(ctx, report)
	if err != nil {
		return nil, err
	}
	shared := &sharedSchedulerLog{log}
	s.epoch, s.log = handle.Epoch, shared
	retire := s.retire
	go func() {
		<-ctx.Done()
		var err error
		if retire != nil {
			err = retire(log)
		} else {
			err = log.Close()
		}
		if err != nil && report != nil && !epoch.IsStale(err) && !errors.Is(err, journal.ErrRunOwned) {
			report(fmt.Errorf("retire scheduler log: %w", err))
		}
	}()
	return shared, nil
}

// epochSchedulerLogs is the process's shared opener: every consumer of the
// scheduler log (the scheduler admission, the terminal hooks) takes it from here.
var epochSchedulerLogs schedulerLogs
