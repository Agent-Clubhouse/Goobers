package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// prRepairCatalog is independent of the mutable schedulerSetup fields. Its
// publication lock is acquired only AFTER interactive policy cancellation/join.
// Session callers never acquire reloader.mu or the interactive policy lock.
type prRepairCatalog struct {
	mu           sync.RWMutex
	snapshot     prRepairSnapshot
	repositories []prRepairRepository
	local        bool
	legacy       *worktree.Manager
	permissions  *interactiveaccess.Service
	installed    bool
}
type prRepairRepository struct {
	identity    providers.RepositoryRef
	unsupported bool
}
type prRepairSnapshot struct {
	enabled      bool
	repositories []prRepairRepository
	managers     []*worktree.Manager
}

// Called during boot, before reload and session execution are admitted.
func newPRRepairCatalog(setup *schedulerSetup, installed bool) *prRepairCatalog {
	c := &prRepairCatalog{installed: installed}
	if setup == nil {
		return c
	}
	c.legacy, c.permissions = setup.LegacyWorktrees, setup.InteractiveAccess
	if setup.Config != nil {
		c.local = setup.Config.Engine == nil
		for _, repo := range setup.Config.Repos {
			c.repositories = append(c.repositories, prRepairRepository{identity: providers.RepositoryRef{Provider: providers.ProviderKind(repo.Provider), Owner: repo.Owner, Project: repo.Project, Name: repo.Name}, unsupported: repo.Pinned() || repo.BaseURL != ""})
		}
	}
	c.snapshot = c.capture(setup.Definitions, setup.Worktrees, setup.WorktreesByGaggle)
	return c
}
func (c *prRepairCatalog) capture(set *instance.ConfigSet, primary *worktree.Manager, managers map[string]*worktree.Manager) prRepairSnapshot {
	snapshot := prRepairSnapshot{enabled: c.local && set != nil, repositories: c.repositories, managers: []*worktree.Manager{primary, c.legacy}}
	if set != nil {
		for _, workflow := range set.Workflows {
			if workflow.Spec.Readiness.ClaimVisibility == "shared" {
				snapshot.enabled = false
			}
		}
	}
	for _, manager := range managers {
		snapshot.managers = append(snapshot.managers, manager)
	}
	return snapshot
}
func (c *prRepairCatalog) use(ctx context.Context, use func(prRepairSnapshot) error) error {
	if c == nil || use == nil {
		return errors.New("PR repair custody catalog unavailable")
	}
	if err := waitPRRepairLock(ctx, c.mu.TryRLock); err != nil {
		return err
	}
	defer c.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return use(c.snapshot)
}
func waitPRRepairLock(ctx context.Context, acquire func() bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if acquire() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Invoked inside publishInteractiveDefinitions's post-join callback. A workflow-
// only reload still fences active repair effects even if its gaggle is unchanged.
func (r *configReloader) publishPRRepairDefinitions(definitions *schedulerDefinitions, publish func() error) error {
	c := r.setup.PRRepairCustody
	if c == nil {
		return publish()
	}
	next := c.capture(definitions.Set, definitions.Worktrees, definitions.WorktreesByGaggle)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := waitPRRepairLock(ctx, c.mu.TryLock); err != nil {
		return err
	}
	defer c.mu.Unlock()
	if err := publish(); err != nil {
		return err
	}
	c.snapshot = next
	if c.permissions != nil {
		c.permissions.SetPRRepairAvailable(c.installed && next.enabled)
	}
	return nil
}
