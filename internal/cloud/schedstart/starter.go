// Package schedstart is the cloud controller's admission path for accepted
// starts. Accepted rows are dispatched through the local scheduler's
// TriggerPreparedOrdinary, so the run conditions (per-workflow and global
// concurrency, rate budgets, provider quota) bind cloud starts exactly as they
// bind local ones. The Starter it hands the scheduler BLOCKS until the run is
// terminal, so the scheduler's slot is held for the run's whole life and
// released only when the run closes.
package schedstart

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Outcome is a run's terminal result as the cloud observes it.
type Outcome struct {
	Phase                                     journal.RunPhase
	FailureStage, FailureCode, FailureMessage string
	NoWork                                    bool
}

// Terminal blocks until the run closes and returns its outcome. It returns an
// error (typically the context's) when the wait is abandoned; it must not
// invent a terminal outcome for a run that is still executing.
type Terminal func(ctx context.Context, runID string) (Outcome, error)

// ErrNotMine reports that the execution for the accepted run belongs to
// another acceptance or a newer epoch, so this epoch holds no slot for it.
var ErrNotMine = errors.New("schedstart: execution belongs to another owner")

// Config binds one epoch's start path.
type Config struct {
	Engine   engine.Starter
	Resolve  func(context.Context, triggerqueue.Record) (engine.RunInput, error)
	Terminal Terminal
	// StartTimeout bounds resolving and starting (not the terminal wait).
	StartTimeout time.Duration
	Report       func(error)
}

// Tracker counts live starter calls so epoch shutdown can wait for them.
type Tracker struct{ wg sync.WaitGroup }

// Wait blocks until every registered dispatch has returned or ctx ends.
func (t *Tracker) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type starter struct {
	config  Config
	record  triggerqueue.Record
	tracker *Tracker
}

// StarterFor returns the scheduler Starter for exactly one accepted record.
// Binding the record here, rather than looking it up by run ID, means a Start
// can never run a different acceptance than the one the scheduler admitted.
func (c Config) StarterFor(record triggerqueue.Record, tracker *Tracker) localscheduler.Starter {
	return &starter{config: c, record: record, tracker: tracker}
}

func (s *starter) RegisterDispatch() func() { return s.tracker.register() }

func (t *Tracker) register() func() {
	if t == nil {
		return func() {}
	}
	t.wg.Add(1)
	var once sync.Once
	return func() { once.Do(t.wg.Done) }
}

func (s *starter) Start(ctx context.Context, req localscheduler.StartRequest) (localscheduler.StartResult, error) {
	runID := strings.TrimPrefix(s.record.ID, "trigger-")
	if req.RunID != runID {
		return localscheduler.StartResult{}, fmt.Errorf("schedstart: scheduler admitted run %q for acceptance %q", req.RunID, s.record.ID)
	}
	startCtx := ctx
	if s.config.StartTimeout > 0 {
		var cancel context.CancelFunc
		startCtx, cancel = context.WithTimeout(ctx, s.config.StartTimeout)
		defer cancel()
	}
	in, err := s.config.Resolve(startCtx, s.record)
	if err != nil {
		return localscheduler.StartResult{}, err
	}
	started, err := s.config.Engine.Start(startCtx, in)
	if err != nil {
		return localscheduler.StartResult{}, err
	}
	if started.NotMine {
		return localscheduler.StartResult{}, ErrNotMine
	}
	// AlreadyRunning without NotMine is this acceptance's own earlier start
	// (a predecessor's): adopt it and hold the slot until it closes.
	outcome, err := s.config.Terminal(ctx, started.RunID)
	if err != nil {
		return localscheduler.StartResult{}, err
	}
	return localscheduler.StartResult{
		Phase: outcome.Phase, NoWork: outcome.NoWork,
		FailureStage: outcome.FailureStage, FailureCode: outcome.FailureCode, FailureMessage: outcome.FailureMessage,
	}, nil
}
