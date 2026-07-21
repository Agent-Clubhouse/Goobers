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
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
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
	protectedBranches, err := retentionProtectedBranches(runsByRoot, setup)
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
		IsBranchProtected: func(root, branch string) (bool, error) {
			_, protected := protectedBranches[root][branch]
			return protected, nil
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

func retentionProtectedBranches(runsByRoot map[string]string, setup *schedulerSetup) (map[string]map[string]struct{}, error) {
	namespaces := map[string]string{}
	if setup.Definitions != nil {
		namespaces = branchNamespacesByGaggle(setup.Definitions)
	}
	protected := make(map[string]map[string]struct{}, len(runsByRoot))
	roots := make([]string, 0, len(runsByRoot))
	for root := range runsByRoot {
		roots = append(roots, root)
	}
	sort.Strings(roots)

	for _, root := range roots {
		protected[root] = make(map[string]struct{})
		runsDir := runsByRoot[root]
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read runs directory %s for retention: %w", runsDir, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			runDir := filepath.Join(runsDir, entry.Name())
			reader, err := journal.OpenRead(runDir)
			if err != nil {
				continue
			}
			phase, err := reader.Phase()
			if err != nil {
				return nil, fmt.Errorf("read phase for retention run %s: %w", entry.Name(), err)
			}
			if terminalRunPhase(phase) {
				continue
			}
			identity, err := reader.Identity()
			if err != nil {
				return nil, fmt.Errorf("read identity for retention run %s: %w", entry.Name(), err)
			}
			events, err := reader.Events()
			if err != nil {
				return nil, fmt.Errorf("read events for retention run %s: %w", entry.Name(), err)
			}
			namespace := providers.NormalizeBranchNamespace(namespaces[identity.Gaggle])
			protected[root][providers.BranchNameIn(namespace, identity.Workflow, identity.RunID)] = struct{}{}
			machine := setup.Machines[localscheduler.WorkflowIdentity{
				Gaggle: identity.Gaggle, Workflow: identity.Workflow,
			}]
			if machine != nil && identity.WorkflowDigest != "" && machine.Digest() == identity.WorkflowDigest {
				if branch := runner.RestoredWorkspaceBranch(events, machine, namespace); branch != "" {
					protected[root][branch] = struct{}{}
				}
				continue
			}
			// Without the pinned machine, protect every plausible binding rather
			// than deleting the one a restored configuration may need to resume.
			protectJournaledWorkspaceBranches(protected[root], events, namespace)
		}
	}
	return protected, nil
}

func protectJournaledWorkspaceBranches(protected map[string]struct{}, events []journal.Event, namespace string) {
	for _, event := range events {
		if event.Type != journal.EventStageFinished {
			continue
		}
		value, ok := event.Outputs[runner.WorkspaceBranchOutput]
		if !ok {
			continue
		}
		branch, ok := value.(string)
		if !ok {
			continue
		}
		branch = strings.TrimSpace(branch)
		if strings.HasPrefix(branch, namespace) {
			protected[branch] = struct{}{}
		}
	}
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
