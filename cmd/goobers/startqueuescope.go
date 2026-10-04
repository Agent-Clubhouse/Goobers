package main

import (
	"context"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/startcontrol"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func newStartQueueControls(layout instance.Layout, queue *triggerqueue.Store, now func() time.Time) *startcontrol.Coordinator {
	return &startcontrol.Coordinator{Queue: queue, Now: now, Archive: func(ctx context.Context, m startcontrol.Metadata) (*apiv1.Gaggle, error) {
		return archivedStartQueuePolicy(ctx, layout, m)
	}}
}

func archivedStartQueuePolicy(ctx context.Context, layout instance.Layout, m startcontrol.Metadata) (*apiv1.Gaggle, error) {
	store, err := executionGenerationStore(layout)
	if err != nil {
		return nil, err
	}
	directory, lease, err := store.Acquire(ctx, m.Scope.Generation)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lease.Release() }()
	set, _, err := loadConfigDirectory(directory)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if !archivedStartSourceExists(set, m) {
		return nil, errors.New("accepted queue source is absent from its archived generation")
	}
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == m.Scope.Gaggle {
			return set.Gaggles[i].DeepCopy(), nil
		}
	}
	return nil, errors.New("accepted queue gaggle is absent from its archived generation")
}
func archivedStartSourceExists(set *instance.ConfigSet, m startcontrol.Metadata) bool {
	if m.ArchiveWorkflow != "" {
		for _, w := range set.Workflows {
			if w.Name == m.ArchiveWorkflow && w.Spec.Gaggle == m.Scope.Gaggle {
				return true
			}
		}
		return false
	}
	if m.ArchiveGoober != "" {
		for _, g := range set.Goobers {
			if g.Name == m.ArchiveGoober && g.Spec.Gaggle == m.Scope.Gaggle {
				return true
			}
		}
	}
	return false
}
func (s *durableTriggerService) prepareQueuedStart(ctx context.Context, record triggerqueue.Record) (bool, error) {
	if s.startControls == nil {
		return true, nil
	}
	return s.startControls.BeforeDispatch(ctx, record)
}
func (s *durableTriggerService) sweepStartControls(ctx context.Context) error {
	if s.startControls == nil {
		return nil
	}
	return s.startControls.Sweep(ctx)
}
