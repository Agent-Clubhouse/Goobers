package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
)

const startupInventoryPageSize = 200

func loadStartupRecoveryInventory(
	ctx context.Context,
	l instance.Layout,
	setup *schedulerSetup,
	tracker *startupPhaseTracker,
	stdout io.Writer,
) ([]string, error) {
	store := setup.ReadModel
	source := "read-model"
	if !setup.ProjectorRestartComplete {
		store = nil
		source = "filesystem-fallback"
	}
	var runDirs []string
	err := runStartupPhase(stdout, tracker, "recovery-run-inventory", "source="+source+" examined=0", func() error {
		var inventoryErr error
		runDirs, inventoryErr = startupRecoveryRunDirs(ctx, l, store, func(examined int, more bool) {
			tracker.update("recovery-run-inventory", fmt.Sprintf("source=%s examined=%d more=%t", source, examined, more))
		})
		if inventoryErr != nil && store != nil {
			source = "filesystem-fallback"
			tracker.update("recovery-run-inventory", "source=filesystem-fallback examined=0")
			runDirs, inventoryErr = startupRecoveryRunDirs(ctx, l, nil, func(examined int, more bool) {
				tracker.update("recovery-run-inventory", fmt.Sprintf("source=%s examined=%d more=%t", source, examined, more))
			})
		}
		return inventoryErr
	})
	return runDirs, err
}

func reconcileStartupRuns(
	ctx context.Context,
	l instance.Layout,
	setup *schedulerSetup,
	sched *localscheduler.Scheduler,
	tracker *startupPhaseTracker,
	stdout io.Writer,
) ([]string, error) {
	recoveryRunDirs, err := loadStartupRecoveryInventory(ctx, l, setup, tracker, stdout)
	if err != nil {
		return nil, fmt.Errorf("inventory startup recovery runs: %w", err)
	}
	runDirs, err := l.RunDirs()
	if err != nil {
		return nil, err
	}
	err = runStartupPhase(stdout, tracker, "scheduler-reconcile", fmt.Sprintf("active-candidates=%d", len(recoveryRunDirs)), func() error {
		return sched.ReconcileRunDirs(runDirs, recoveryRunDirs, time.Now())
	})
	return recoveryRunDirs, err
}

func resumeStartupRuns(
	ctx context.Context,
	l instance.Layout,
	setup *schedulerSetup,
	guards *engineRunGuards,
	sched *localscheduler.Scheduler,
	wg *sync.WaitGroup,
	recoveryRunDirs []string,
	tracker *startupPhaseTracker,
	stdout io.Writer,
) (resumed, warned, reattached []string, err error) {
	err = runStartupPhase(stdout, tracker, "crash-resume", fmt.Sprintf("candidates=%d", len(recoveryRunDirs)), func() error {
		var resumeErr error
		resumed, warned, reattached, resumeErr = resumeInterruptedRunsWithRunners(
			ctx, l, setup.Runners, setup.LegacyRunner, setup.RunnerRegistry, guards,
			setup.Machines, setup.GooberDigests, setup.RepoRefs, setup.InstanceLog,
			setup.Telemetry, setup.RollupDB, setup.Watermarks, sched.ReleaseReconciled,
			wg, recoveryRunDirs,
		)
		return resumeErr
	})
	return
}

func renewResumedClaimsAtStartup(
	ctx context.Context,
	l instance.Layout,
	claimLiveness localscheduler.RunLivenessProbe,
	resumed int,
	tracker *startupPhaseTracker,
	stdout io.Writer,
) error {
	var renewErr error
	_ = runStartupPhase(stdout, tracker, "resumed-claim-renewal", fmt.Sprintf("runs=%d", resumed), func() error {
		_, _, renewErr = renewLiveClaims(ctx, l, claimLiveness, DefaultClaimLease)
		return renewErr
	})
	return renewErr
}

func runVoidStartupPhase(stdout io.Writer, tracker *startupPhaseTracker, phase string, fn func()) {
	_ = runStartupPhase(stdout, tracker, phase, "", func() error {
		fn()
		return nil
	})
}

func reconcileStartupEphemeralTemp(setup *schedulerSetup, tracker *startupPhaseTracker, stdout io.Writer) {
	runVoidStartupPhase(stdout, tracker, "ephemeral-temp-reconcile", func() {
		sweepOrphanedEphemeralTmp(setup.Config, setup.InstanceLog)
	})
}

func reconcileStartupClaimAdmin(
	l instance.Layout,
	setup *schedulerSetup,
	recoverExpiredClaims func(time.Time) ([]localscheduler.ClaimEntry, error),
	tracker *startupPhaseTracker,
	stdout io.Writer,
) error {
	return runStartupPhase(stdout, tracker, "claim-admin-reconcile", "", func() error {
		return sweepPendingClaimAdminRequests(l.SchedulerDir(), setup.InstanceLog, time.Now, recoverExpiredClaims)
	})
}

func startFleetConnectorPhase(
	ctx context.Context,
	root string,
	tracker *startupPhaseTracker,
	stdout io.Writer,
) (<-chan error, bool, error) {
	var done <-chan error
	var started bool
	var startErr error
	_ = runStartupPhase(stdout, tracker, "fleet-connector-start", "", func() error {
		done, started, startErr = startDaemonFleetConnector(ctx, root)
		return startErr
	})
	return done, started, startErr
}

func recoveryRunCandidates(ctx context.Context, l instance.Layout, explicit ...[]string) ([]string, error) {
	if len(explicit) > 0 {
		return explicit[0], nil
	}
	return startupRecoveryRunDirsFromFilesystem(ctx, l, nil)
}

func startupRecoveryRunDirs(
	ctx context.Context,
	l instance.Layout,
	store *readmodel.Store,
	progress func(examined int, more bool),
) ([]string, error) {
	marked, err := markedRecoveryRunDirs(l)
	if err != nil {
		return nil, err
	}
	if store == nil {
		return startupRecoveryRunDirsFromFilesystem(ctx, l, progress)
	}

	options := readmodel.ListOptions{
		Phase: journal.PhaseRunning,
		Limit: startupInventoryPageSize,
	}
	runDirs := append([]string(nil), marked...)
	seen := make(map[string]struct{})
	for _, runDir := range marked {
		seen[filepath.Base(runDir)] = struct{}{}
	}
	for {
		page, err := store.ListRuns(ctx, options)
		if err != nil {
			return nil, fmt.Errorf("list projected non-terminal runs: %w", err)
		}
		for _, row := range page.Runs {
			if _, duplicate := seen[row.RunID]; duplicate {
				continue
			}
			runDir, err := l.FindRunDir(row.RunID)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			seen[row.RunID] = struct{}{}
			runDirs = append(runDirs, runDir)
			if _, err := journal.MarkRunActive(filepath.Dir(runDir), row.RunID); err != nil {
				return nil, err
			}
		}
		if progress != nil {
			progress(len(runDirs), page.HasMore)
		}
		if !page.HasMore {
			return runDirs, nil
		}
		options.Cursor = page.Next
	}
}

func markedRecoveryRunDirs(l instance.Layout) ([]string, error) {
	roots, err := l.RunDirs()
	if err != nil {
		return nil, err
	}
	legacyRoot := l.RunsDir()
	legacyIncluded := false
	for _, root := range roots {
		if filepath.Clean(root) == filepath.Clean(legacyRoot) {
			legacyIncluded = true
			break
		}
	}
	if !legacyIncluded {
		roots = append(roots, legacyRoot)
	}
	var runDirs []string
	seen := make(map[string]struct{})
	for _, root := range roots {
		marked, err := journal.ActiveRunDirs(root)
		if err != nil {
			return nil, err
		}
		for _, markedRunDir := range marked {
			runID := filepath.Base(markedRunDir)
			runDir, err := l.FindRunDir(runID)
			if errors.Is(err, fs.ErrNotExist) {
				if clearErr := journal.ClearRunActive(markedRunDir); clearErr != nil {
					return nil, clearErr
				}
				continue
			}
			if err != nil {
				return nil, err
			}
			if filepath.Clean(markedRunDir) != filepath.Clean(runDir) {
				if _, err := journal.MarkRunActive(filepath.Dir(runDir), runID); err != nil {
					return nil, err
				}
				if err := journal.ClearRunActive(markedRunDir); err != nil {
					return nil, err
				}
			}
			if _, duplicate := seen[runID]; duplicate {
				continue
			}
			seen[runID] = struct{}{}
			runDirs = append(runDirs, runDir)
		}
	}
	return runDirs, nil
}

func startupRecoveryRunDirsFromFilesystem(
	ctx context.Context,
	l instance.Layout,
	progress func(examined int, more bool),
) ([]string, error) {
	roots, err := l.RunDirs()
	if err != nil {
		return nil, err
	}
	var runDirs []string
	for rootIndex, root := range roots {
		entries, err := os.ReadDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read runs directory %s: %w", root, err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if entry.IsDir() {
				runDirs = append(runDirs, filepath.Join(root, entry.Name()))
			}
		}
		if progress != nil {
			progress(len(runDirs), rootIndex+1 < len(roots))
		}
	}
	return runDirs, nil
}
