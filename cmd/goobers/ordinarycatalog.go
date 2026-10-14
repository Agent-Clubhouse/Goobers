package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
)

type ordinaryStartCatalog struct {
	mu         sync.RWMutex
	generation string
	entries    []localscheduler.WorkflowEntry
	store      configgeneration.Store
}

func (c *ordinaryStartCatalog) capture(ctx context.Context, request startintent.Request) (startintent.Target, func(), error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var target startintent.Target
	for _, entry := range c.entries {
		if entry.Workflow != request.Workflow || (request.Gaggle != "" && entry.Gaggle != request.Gaggle) {
			continue
		}
		if target.Workflow != "" {
			return target, nil, fmt.Errorf("workflow %q is ambiguous; name its gaggle: %w", request.Workflow, startintent.ErrInvalid)
		}
		if entry.DisabledReason != "" {
			return target, nil, fmt.Errorf("%s: %w", entry.DisabledReason, startintent.ErrInvalid)
		}
		target = startintent.Target{Gaggle: entry.Gaggle, Workflow: entry.Workflow, ConfigGeneration: c.generation, WorkflowDigest: entry.WorkflowDigest, GooberDigest: entry.GooberDigest}
	}
	if err := target.Validate(); err != nil {
		return target, nil, fmt.Errorf("workflow %q has no applied execution target: %w", request.Workflow, errors.Join(err, startintent.ErrInvalid))
	}
	_, lease, err := c.store.Acquire(ctx, target.ConfigGeneration)
	if err != nil {
		return target, nil, err
	}
	return target, func() { _ = lease.Release() }, nil
}

func (r *configReloader) publishOrdinaryDefinitions(definitions *schedulerDefinitions, publish func() error) error {
	c := r.setup.OrdinaryCatalog
	if c == nil {
		return publish()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := publish(); err != nil {
		return err
	}
	c.generation = definitions.ExecutionGeneration
	c.entries = append([]localscheduler.WorkflowEntry(nil), definitions.Entries...)
	return nil
}
