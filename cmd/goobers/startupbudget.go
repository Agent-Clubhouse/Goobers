package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/worktree"
)

const (
	// The live accumulation measurement in #5199 was 51 minutes for 1,481
	// recovery candidates (2.07s/candidate). Round that observed rate up so
	// the budget grows with the work that made startup slow without requiring
	// operators to hand-raise one fixed probe threshold after every incident.
	startupBudgetPerCandidate = 3 * time.Second
	startupBudgetWarningPct   = 80
)

type startupAccumulation struct {
	Worktrees    int
	RecoveryRuns int
}

func (a startupAccumulation) total() int {
	return a.Worktrees + a.RecoveryRuns
}

func deriveStartupBudget(floor time.Duration, accumulation startupAccumulation) time.Duration {
	if floor < 0 {
		floor = 0
	}
	return floor + time.Duration(accumulation.total())*startupBudgetPerCandidate
}

func startupBudgetState(elapsed, budget time.Duration) (string, float64) {
	if budget <= 0 {
		return "unavailable", 0
	}
	used := float64(elapsed) / float64(budget) * 100
	switch {
	case elapsed >= budget:
		return "exceeded", used
	case used >= startupBudgetWarningPct:
		return "approaching", used
	default:
		return "within-budget", used
	}
}

func measureWorktreeAccumulation(managers map[string]*worktree.Manager, legacy *worktree.Manager) (int, error) {
	roots := make(map[string]struct{}, len(managers)+1)
	for _, manager := range managers {
		if manager != nil {
			roots[filepath.Clean(manager.Root)] = struct{}{}
		}
	}
	if legacy != nil {
		roots[filepath.Clean(legacy.Root)] = struct{}{}
	}

	total := 0
	for root := range roots {
		count, err := worktreeAccumulationAt(root)
		if err != nil {
			return 0, err
		}
		total += count
	}
	return total, nil
}

func worktreeAccumulationAt(root string) (int, error) {
	count, err := worktree.CountReapCandidates(root)
	if err != nil {
		return 0, fmt.Errorf("measure startup worktree accumulation at %s: %w", root, err)
	}
	return count, nil
}
