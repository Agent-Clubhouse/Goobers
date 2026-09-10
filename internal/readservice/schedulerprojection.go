package readservice

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
)

var ErrSchedulerProjectorStopTimeout = errors.New("readservice: scheduler-state projector shutdown timed out")

const (
	defaultSchedulerProjectionInterval = 5 * time.Second
	schedulerProjectorStopTimeout      = 5 * time.Second
)

type workflowSchedulerProjection struct {
	engineFallbacks map[localscheduler.WorkflowIdentity]readmodel.EngineFallback
	refillBlocked   map[localscheduler.WorkflowIdentity]string
}

type schedulerStateProjector struct {
	service  *Local
	interval time.Duration

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
	cancel    context.CancelFunc
}

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
		return
	}
	fallbacks := make(map[localscheduler.WorkflowIdentity]readmodel.EngineFallback, len(projected.engineFallbacks.items))
	for key, fallback := range projected.engineFallbacks.items {
		fallbacks[key] = fallback
	}
	p.service.workflowSchedulerState.Store(&workflowSchedulerProjection{
		engineFallbacks: fallbacks,
		refillBlocked:   maps.Clone(projected.refillBlocked),
	})
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
