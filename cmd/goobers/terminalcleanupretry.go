package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/boundedagg"
	"github.com/goobers/goobers/internal/workcopyroot"
	"github.com/goobers/goobers/internal/worktree"
)

var (
	terminalCleanupRetryInterval = time.Minute
	terminalCleanupRetryTimeout  = 2 * time.Minute
)

const terminalCleanupRetryBatch = 8

type terminalCleanupRetryTarget struct {
	name      string
	rootKey   string
	displaced bool
	manager   *worktree.Manager
}

type terminalCleanupRetryState struct {
	next    int
	cursors map[*worktree.Manager]string
}

// terminalCleanupRetryRegistry is the reload-safe source of managers scanned
// by the prompt retry loop. Config reload can replace a manager when a
// gaggle's workcopies root or pinned mode changes, so the loop must take a
// fresh atomic snapshot for every pass rather than retaining boot managers.
type terminalCleanupRetryRegistry struct {
	mu      sync.RWMutex
	targets []terminalCleanupRetryTarget
}

func newTerminalCleanupRetryRegistry(setup *schedulerSetup) *terminalCleanupRetryRegistry {
	r := &terminalCleanupRetryRegistry{}
	r.Replace(setup.WorktreesByGaggle, setup.LegacyWorktrees)
	return r
}

func (r *terminalCleanupRetryRegistry) Replace(byGaggle map[string]*worktree.Manager, legacy *worktree.Manager) {
	if r == nil {
		return
	}
	names := make([]string, 0, len(byGaggle))
	for name := range byGaggle {
		names = append(names, name)
	}
	sort.Strings(names)
	r.mu.Lock()
	defer r.mu.Unlock()
	previous := append([]terminalCleanupRetryTarget(nil), r.targets...)
	targets := make([]terminalCleanupRetryTarget, 0, len(names)+1+len(previous))
	currentTargets := make(map[string]bool, len(names)+1)
	for _, name := range names {
		if manager := byGaggle[name]; manager != nil {
			target := newTerminalCleanupRetryTarget("gaggle:"+name, manager)
			targets = append(targets, target)
			currentTargets[target.rootKey] = true
		}
	}
	if legacy != nil {
		target := newTerminalCleanupRetryTarget("legacy", legacy)
		targets = append(targets, target)
		currentTargets[target.rootKey] = true
	}
	// Scheduler reload deliberately lets already-dispatched runs finish on
	// their old Runner. If that Runner later surrenders cleanup in its old
	// workcopies root, the durable queue must remain reachable. Retain one
	// scanner per displaced physical root for this daemon's lifetime. Any
	// current manager rooted there already discovers the same durable queue;
	// the retry loop invokes its targets sequentially.
	retainedTargets := make(map[string]bool, len(previous))
	for _, target := range previous {
		if currentTargets[target.rootKey] || retainedTargets[target.rootKey] {
			continue
		}
		if !target.displaced {
			target.name += " (displaced)"
			target.displaced = true
		}
		targets = append(targets, target)
		retainedTargets[target.rootKey] = true
	}
	r.targets = targets
}

func newTerminalCleanupRetryTarget(name string, manager *worktree.Manager) terminalCleanupRetryTarget {
	root, err := workcopyroot.Key(manager.Root)
	if err != nil {
		// NewManager stores an absolute Root, so this is defensive only. Keep
		// the raw value distinct rather than silently merging unknown roots.
		root = manager.Root
	}
	return terminalCleanupRetryTarget{name: name, rootKey: root, manager: manager}
}

func (r *terminalCleanupRetryRegistry) Snapshot() []terminalCleanupRetryTarget {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	targets := append([]terminalCleanupRetryTarget(nil), r.targets...)
	r.mu.RUnlock()
	return targets
}

// startTerminalCleanupRetry starts the prompt, bounded counterpart to the
// six-hour broad worktree reaper. It begins only after readiness, runs one
// immediate pass, and joins daemon shutdown through the returned channel.
func startTerminalCleanupRetry(ctx context.Context, registry *terminalCleanupRetryRegistry, reporter *sweepErrorReporter, ready bool) <-chan struct{} {
	done := make(chan struct{})
	if !ready {
		close(done)
		return done
	}
	state := &terminalCleanupRetryState{cursors: make(map[*worktree.Manager]string)}
	go func() {
		defer close(done)
		ticker := time.NewTicker(terminalCleanupRetryInterval)
		defer ticker.Stop()
		for {
			passCtx, cancel := context.WithTimeout(ctx, terminalCleanupRetryTimeout)
			err := state.run(passCtx, registry.Snapshot(), terminalCleanupRetryBatch)
			cancel()
			if ctx.Err() == nil {
				reporter.report(err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

// runTerminalCleanupRetryFinal gives every retained manager one bounded turn
// after the daemon has drained its in-flight runs. A displaced Runner is
// allowed to finish during drain and can create cleanup-pending only then;
// stopping the minute loop before drain would strand that durable queue when
// the next process starts from the replacement configuration.
func runTerminalCleanupRetryFinal(registry *terminalCleanupRetryRegistry, reporter *sweepErrorReporter, ready bool) {
	if !ready {
		return
	}
	targets := registry.Snapshot()
	state := &terminalCleanupRetryState{}
	ctx, cancel := context.WithTimeout(context.Background(), terminalCleanupRetryTimeout)
	defer cancel()
	for range len(targets) {
		if ctx.Err() != nil {
			break
		}
		err := state.run(ctx, targets, terminalCleanupRetryBatch)
		reporter.report(err)
	}
}

func (s *terminalCleanupRetryState) run(ctx context.Context, targets []terminalCleanupRetryTarget, limit int) error {
	if limit <= 0 || len(targets) == 0 {
		return nil
	}
	if s.cursors == nil {
		s.cursors = make(map[*worktree.Manager]string)
	}
	start := s.next % len(targets)
	remaining := limit
	var retryErrs []error
	for offset := 0; offset < len(targets) && remaining > 0; offset++ {
		if err := ctx.Err(); err != nil {
			return boundedagg.Join(append(retryErrs, err)...)
		}
		index := (start + offset) % len(targets)
		target := targets[index]
		report, err := target.manager.RetryCleanupPending(ctx, worktree.CleanupRetryOptions{
			After: s.cursors[target.manager], Limit: remaining,
		})
		s.cursors[target.manager] = report.Next
		remaining -= report.Attempted
		s.next = (index + 1) % len(targets)
		if err != nil {
			retryErrs = append(retryErrs, fmt.Errorf("%s: %w", target.name, err))
		}
		for _, warning := range report.Warnings {
			retryErrs = append(retryErrs, fmt.Errorf("%s cleanup %s: %w", target.name, warning.Path, warning.Err))
		}
	}
	return boundedagg.Join(retryErrs...)
}
