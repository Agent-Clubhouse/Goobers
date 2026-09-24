package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	repositories, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("measure startup worktree accumulation at %s: %w", root, err)
	}

	total := 0
	for _, repository := range repositories {
		if !repository.IsDir() || repository.Name() == "scratch" {
			continue
		}
		repositoryRoot := filepath.Join(root, repository.Name())
		markers, err := countAccumulationEntries(filepath.Join(repositoryRoot, "markers"), false)
		if err != nil {
			return 0, err
		}
		runs, err := countAccumulationEntries(filepath.Join(repositoryRoot, "runs"), true)
		if err != nil {
			return 0, err
		}
		// Markers and run directories normally describe the same worktree.
		// The larger side includes whichever crash-only shape accumulated
		// (marker without directory, or markerless directory) without counting
		// every healthy pair twice.
		if runs > markers {
			markers = runs
		}
		total += markers
	}
	return total, nil
}

func countAccumulationEntries(path string, directories bool) (int, error) {
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("measure startup accumulation at %s: %w", path, err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() == directories && (directories || strings.HasSuffix(entry.Name(), ".json")) {
			count++
		}
	}
	return count, nil
}
