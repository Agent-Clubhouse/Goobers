package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func pruneConfiguredRetention(ctx context.Context, l instance.Layout, setup *schedulerSetup, stdout, stderr io.Writer) error {
	cfg := setup.Config.Retention
	if !cfg.Enabled && !cfg.DryRun {
		return nil
	}
	maxAge, err := cfg.RetainedWorktreeMaxAgeDuration()
	if err != nil {
		return err
	}

	managers, runsByRoot, err := retentionManagers(l, setup)
	if err != nil {
		return err
	}
	results, warnings, err := worktree.PruneRetained(ctx, managers, worktree.RetentionOptions{
		Delete:           cfg.Enabled && !cfg.DryRun,
		MaxRetainedBytes: cfg.MaxRetainedWorktreeBytes,
		MaxAge:           maxAge,
		IsTerminalFailure: func(root, worktreeID, ownerRunID string) (bool, error) {
			phase, found, err := retainedWorktreePhase(runsByRoot[root], worktreeID, ownerRunID)
			return found && terminalFailurePhase(phase), err
		},
		IsRunTerminal: func(root, runID string) (bool, error) {
			phase, found, err := readRunPhase(runsByRoot[root], runID)
			return found && terminalRunPhase(phase), err
		},
	})
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		pf(stdout, "warning: skipped retention candidate %q: %v\n", warning.Path, warning.Err)
	}
	for _, result := range results {
		target := retentionResultTarget(result)
		switch {
		case result.Err != nil:
			pf(stderr, "warning: retention deletion failed rule=%s kind=%s %s: %v\n", result.Rule, result.Kind, target, result.Err)
		case result.DryRun:
			pf(stdout, "retention candidate rule=%s kind=%s %s\n", result.Rule, result.Kind, target)
		case result.Deleted:
			pf(stdout, "retention deleted rule=%s kind=%s %s reclaimedBytes=%d\n", result.Rule, result.Kind, target, result.BytesReclaimed)
		}
	}
	return nil
}

func retentionManagers(l instance.Layout, setup *schedulerSetup) ([]*worktree.Manager, map[string]string, error) {
	var managers []*worktree.Manager
	runsByRoot := make(map[string]string)
	gaggles := make([]string, 0, len(setup.WorktreesByGaggle))
	for gaggle := range setup.WorktreesByGaggle {
		gaggles = append(gaggles, gaggle)
	}
	sort.Strings(gaggles)
	for _, gaggle := range gaggles {
		manager := setup.WorktreesByGaggle[gaggle]
		if err := addRetentionManager(&managers, runsByRoot, manager, l.ForGaggle(gaggle).RunsDir()); err != nil {
			return nil, nil, err
		}
	}
	if err := addRetentionManager(&managers, runsByRoot, setup.LegacyWorktrees, l.RunsDir()); err != nil {
		return nil, nil, err
	}
	return managers, runsByRoot, nil
}

func addRetentionManager(managers *[]*worktree.Manager, runsByRoot map[string]string, manager *worktree.Manager, runsDir string) error {
	if manager == nil {
		return nil
	}
	if existing, ok := runsByRoot[manager.Root]; ok {
		if filepath.Clean(existing) != filepath.Clean(runsDir) {
			return fmt.Errorf("worktree root %s maps to both %s and %s", manager.Root, existing, runsDir)
		}
		return nil
	}
	runsByRoot[manager.Root] = runsDir
	*managers = append(*managers, manager)
	return nil
}

func retainedWorktreePhase(runsDir, worktreeID, ownerRunID string) (journal.RunPhase, bool, error) {
	if ownerRunID != "" {
		return readRunPhase(runsDir, ownerRunID)
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read runs directory: %w", err)
	}
	var owner string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runID := entry.Name()
		if worktreeID != runID && !strings.HasPrefix(worktreeID, runID+"-") {
			continue
		}
		if len(runID) > len(owner) {
			owner = runID
		}
	}
	if owner == "" {
		return "", false, nil
	}
	return readRunPhase(runsDir, owner)
}

func readRunPhase(runsDir, runID string) (journal.RunPhase, bool, error) {
	runDir := filepath.Join(runsDir, runID)
	if _, err := os.Stat(runDir); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return "", false, err
	}
	phase, err := reader.Phase()
	return phase, err == nil, err
}

func terminalRunPhase(phase journal.RunPhase) bool {
	switch phase {
	case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
		return true
	default:
		return false
	}
}

func terminalFailurePhase(phase journal.RunPhase) bool {
	switch phase {
	case journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
		return true
	default:
		return false
	}
}

func retentionResultTarget(result worktree.RetentionResult) string {
	if result.Kind == worktree.RetentionKindBranch {
		return fmt.Sprintf("run=%q branch=%q repository=%q", result.RunID, result.Branch, result.RepositoryPath)
	}
	return fmt.Sprintf("run=%q worktree=%q path=%q bytes=%d", result.RunID, result.WorktreeID, result.Path, result.Bytes)
}
