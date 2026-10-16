package readservice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
)

// ErrSchedulerProjectorStopTimeout means the background scheduler projection did not stop promptly.
var ErrSchedulerProjectorStopTimeout = errors.New("readservice: scheduler-state projector shutdown timed out")

const (
	defaultSchedulerProjectionInterval = 5 * time.Second
	schedulerProjectorStopTimeout      = 5 * time.Second
)

type workflowSchedulerProjection struct {
	engineFallbacks map[localscheduler.WorkflowIdentity]readmodel.EngineFallback
	refillBlocked   map[localscheduler.WorkflowIdentity]string
	blockerClears   map[string]time.Time
}

type schedulerStateProjector struct {
	service  *Local
	interval time.Duration

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
	cancel    context.CancelFunc

	logger  *slog.Logger
	lastErr string
}

// StartSchedulerStateProjector starts periodic scheduler-state projection and returns its stop function.
func (s *Local) StartSchedulerStateProjector(interval time.Duration) func() error {
	if interval <= 0 {
		interval = defaultSchedulerProjectionInterval
	}
	projector := &schedulerStateProjector{
		service:  s,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if !s.schedulerProjector.CompareAndSwap(nil, projector) {
		projector = s.schedulerProjector.Load()
	}
	projector.start()
	return projector.stopAndWait
}

func (p *schedulerStateProjector) start() {
	p.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		p.cancel = cancel
		go func() {
			defer close(p.done)
			p.refresh(ctx)
			ticker := time.NewTicker(p.interval)
			defer ticker.Stop()
			for {
				select {
				case <-p.stop:
					return
				case <-ticker.C:
					p.refresh(ctx)
				}
			}
		}()
	})
}

func (p *schedulerStateProjector) refresh(ctx context.Context) {
	projected, err := p.service.instanceLog.snapshot(
		ctx,
		p.service.sources.Layout.SchedulerDir(),
	)
	if err != nil {
		p.recordRefreshFailure(ctx, err)
		return
	}
	if p.lastErr != "" {
		p.lastErr = ""
		p.log().Info("scheduler-state projection refresh recovered")
	}
	fallbacks := make(map[localscheduler.WorkflowIdentity]readmodel.EngineFallback, len(projected.engineFallbacks.items))
	for key, fallback := range projected.engineFallbacks.items {
		fallbacks[key] = fallback
	}
	p.service.workflowSchedulerState.Store(&workflowSchedulerProjection{
		engineFallbacks: fallbacks,
		refillBlocked:   maps.Clone(projected.refillBlocked),
		blockerClears:   maps.Clone(projected.blockerClears),
	})
}

// recordRefreshFailure logs only when the error changes so a persistent
// failure does not flood the log every interval; shutdown cancellation is silent.
func (p *schedulerStateProjector) recordRefreshFailure(ctx context.Context, err error) {
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return
	}
	msg := err.Error()
	if msg == p.lastErr {
		return
	}
	p.lastErr = msg
	p.log().Error("scheduler-state projection refresh failed; serving last good projection", "error", err)
}

func (p *schedulerStateProjector) log() *slog.Logger {
	if p.logger != nil {
		return p.logger
	}
	return slog.Default()
}

func (p *schedulerStateProjector) stopAndWait() error {
	p.stopOnce.Do(func() {
		close(p.stop)
		if p.cancel != nil {
			p.cancel()
		}
	})
	timer := time.NewTimer(schedulerProjectorStopTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("%w after %s", ErrSchedulerProjectorStopTimeout, schedulerProjectorStopTimeout)
	}
}

func (s *Local) projectedWorkflowSchedulerState() *workflowSchedulerProjection {
	return s.workflowSchedulerState.Load()
}

func (s *Local) workflowSchedulerSnapshot(ctx context.Context) (*workflowSchedulerProjection, error) {
	if projected := s.projectedWorkflowSchedulerState(); projected != nil {
		return projected, nil
	}
	state, err := s.instanceLog.snapshot(ctx, s.sources.Layout.SchedulerDir())
	if err != nil {
		return nil, err
	}
	fallbacks := make(map[localscheduler.WorkflowIdentity]readmodel.EngineFallback, len(state.engineFallbacks.items))
	for key, fallback := range state.engineFallbacks.items {
		fallbacks[key] = fallback
	}
	return &workflowSchedulerProjection{
		engineFallbacks: fallbacks,
		refillBlocked:   maps.Clone(state.refillBlocked),
		blockerClears:   maps.Clone(state.blockerClears),
	}, nil
}
