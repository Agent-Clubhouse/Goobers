package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// errRetentionSweepAlreadyRunning is returned by retentionSweepGate.run when
// a sweep is already in flight. Not a failure: the caller's own error
// reporter treats it as a no-op rather than a degraded-maintenance failure
// (#4373's "concurrent ... sweep requests coalesce or return a typed
// already-running result").
var errRetentionSweepAlreadyRunning = errors.New("worktree retention sweep already running")

// retentionSweepGate ensures at most one worktree/branch retention sweep
// runs at a time, shared across the startup-triggered background sweep and
// the periodic ticker (#4373: "startup, periodic, and operator-triggered
// sweeps must share the same exclusion mechanism"). A sweep already running
// wins; a concurrent caller is told so immediately rather than queuing
// behind it or blocking.
type retentionSweepGate struct {
	mu      sync.Mutex
	running bool
}

func (g *retentionSweepGate) run(fn func() error) error {
	g.mu.Lock()
	if g.running {
		g.mu.Unlock()
		return errRetentionSweepAlreadyRunning
	}
	g.running = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.running = false
		g.mu.Unlock()
	}()
	return fn()
}

// startDeferredRetentionSweep launches the broad worktree/branch retention
// sweep deferred until API readiness (#4373) when ready is true, coalescing
// against the periodic ticker via gate so at most one sweep ever runs at a
// time — a concurrent already-running result is not a failure. It always
// returns a channel that closes once the sweep (if any) is done, closed
// immediately when ready is false, so a caller can unconditionally wait on
// it without knowing which case applied.
func startDeferredRetentionSweep(ctx context.Context, l instance.Layout, setup *schedulerSetup, gate *retentionSweepGate, reporter *sweepErrorReporter, ready bool) <-chan struct{} {
	done := make(chan struct{})
	if !ready {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		err := gate.run(func() error {
			return pruneConfiguredRetention(ctx, l, setup, io.Discard, io.Discard)
		})
		if !errors.Is(err, errRetentionSweepAlreadyRunning) {
			reporter.report(err)
		}
	}()
	return done
}

// journalGraceAge bounds how long a retained worktree whose owning run
// journal has vanished entirely (e.g. telemetry retention deleted it first)
// is tolerated before it becomes prunable under RetentionRuleJournalGrace
// (#2052). Unlike MaxRetainedWorktreeBytes/RetainedWorktreeMaxAge, this is
// not operator-configurable: a journal-less retained worktree is always a
// bug (nothing can ever authorize it via IsTerminalFailure again), so
// there's no legitimate reason for an operator to want it kept forever. Var,
// not const, so tests can shrink it rather than waiting out a real 24 hours.
var journalGraceAge = 24 * time.Hour

// sweepWorktreeRetention re-runs crash-orphan reaping (Manager.Reap) and
// configured retention (pruneConfiguredRetention) for every gaggle plus the
// legacy manager. It is the periodic counterpart to the synchronous startup
// block in runUpContext: both previously ran once at daemon start only
// (#2052), so a crash orphan or a kept failure worktree that appeared after
// startup sat on disk until the daemon's next restart. Writers are
// io.Discard, matching every other periodic sweep in runUpContext — a
// background goroutine must never write to stdout/stderr, which are shared
// with the main goroutine's own writes for the daemon's entire lifetime and
// are not safe for concurrent use.
func sweepWorktreeRetention(ctx context.Context, l instance.Layout, setup *schedulerSetup) error {
	for gaggle, manager := range setup.WorktreesByGaggle {
		if _, _, err := manager.Reap(ctx, worktree.ReapOptions{
			IsRunTerminal: worktreeRunTerminal(l.ForGaggle(gaggle).RunsDir()),
		}); err != nil {
			return fmt.Errorf("reap worktrees for gaggle %s: %w", gaggle, err)
		}
	}
	if setup.LegacyWorktrees != nil {
		if _, _, err := setup.LegacyWorktrees.Reap(ctx, worktree.ReapOptions{
			IsRunTerminal: worktreeRunTerminal(l.RunsDir()),
		}); err != nil {
			return fmt.Errorf("reap legacy worktrees: %w", err)
		}
	}
	return pruneConfiguredRetention(ctx, l, setup, io.Discard, io.Discard)
}

func sweepMigrationBackups(l instance.Layout, setup *schedulerSetup, now time.Time) error {
	if err := journal.PruneMigrationBackups(l.RunsDir(), now); err != nil {
		return err
	}
	for gaggle := range setup.WorktreesByGaggle {
		if err := journal.PruneMigrationBackups(l.ForGaggle(gaggle).RunsDir(), now); err != nil {
			return fmt.Errorf("prune migration backups for gaggle %s: %w", gaggle, err)
		}
	}
	return nil
}

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
		JournalMissing: func(root, worktreeID, ownerRunID string) (bool, error) {
			return retainedWorktreeJournalMissing(runsByRoot[root], worktreeID, ownerRunID)
		},
		JournalGraceAge: journalGraceAge,
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
				if errors.Is(err, journal.ErrNotRunDirectory) {
					continue
				}
				return nil, fmt.Errorf("open retention run %s: %w", entry.Name(), err)
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
	owner, err := resolveRetainedWorktreeOwner(runsDir, worktreeID, ownerRunID)
	if err != nil {
		return "", false, err
	}
	if owner == "" {
		return "", false, nil
	}
	return readRunPhase(runsDir, owner)
}

// retainedWorktreeJournalMissing reports whether the retained worktree's
// owning run journal directory does not exist under runsDir at all — #2052's
// grace-window trigger. This is deliberately narrower than "found=false" from
// retainedWorktreePhase, which also covers a journal that exists but is
// unreadable or errors reading its phase; only a confirmed-absent directory
// (e.g. already removed by telemetry retention) counts as "missing" here, so
// a merely-corrupt-but-present journal is never treated as gone.
func retainedWorktreeJournalMissing(runsDir, worktreeID, ownerRunID string) (bool, error) {
	owner, err := resolveRetainedWorktreeOwner(runsDir, worktreeID, ownerRunID)
	if err != nil {
		return false, err
	}
	if owner == "" {
		// No owning run directory resolves at all — via a stamped ownerRunID
		// that doesn't exist, or (for legacy markers with no ownerRunID) no
		// runsDir entry whose name prefixes worktreeID. Either way, there is
		// no journal left to ever authorize this worktree, which is exactly
		// what the grace window exists to eventually resolve.
		return true, nil
	}
	if _, err := os.Stat(filepath.Join(runsDir, owner)); err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// resolveRetainedWorktreeOwner identifies the run ID that owns a retained
// worktree marker: the stamped OwnerRunID when present, or (for legacy
// markers predating that field) the longest runsDir entry whose name
// prefixes the worktree ID. Returns "" — not an error — when no owner can be
// resolved by either means.
func resolveRetainedWorktreeOwner(runsDir, worktreeID, ownerRunID string) (string, error) {
	if ownerRunID != "" {
		return ownerRunID, nil
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read runs directory: %w", err)
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
	return owner, nil
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
