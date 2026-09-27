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

// startupInventoryCounts breaks the candidate set down by where its entries
// came from. #5199 needed exactly this and did not have it: a phase reporting
// only "candidates=1481" cannot distinguish 1,481 durable active markers left
// behind by unfinished cleanups from a read model projecting 1,481 runs as
// still running, and the two have different fixes.
type startupInventoryCounts struct {
	// Marked are durable on-disk active markers — runs with recovery work
	// recorded as outstanding.
	Marked int
	// Discovered are candidates the source added on top: rows the read model
	// projects as non-terminal, or directories the filesystem fallback
	// scanned.
	Discovered int
	More       bool
}

func (c startupInventoryCounts) String() string {
	return fmt.Sprintf("marked=%d discovered=%d candidates=%d more=%t", c.Marked, c.Discovered, c.Marked+c.Discovered, c.More)
}

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
	var counts startupInventoryCounts
	tracker.beginRecoveryAccumulation()
	observe := func(seen startupInventoryCounts) {
		counts = seen
		tracker.observeRecoveryAccumulation(seen.Marked + seen.Discovered)
		tracker.update("recovery-run-inventory", fmt.Sprintf("source=%s %s", source, seen))
	}
	err := runStartupPhase(stdout, tracker, "recovery-run-inventory", "source="+source, func() error {
		var inventoryErr error
		runDirs, inventoryErr = startupRecoveryRunDirs(ctx, l, store, observe)
		if inventoryErr != nil && store != nil {
			source = "filesystem-fallback"
			counts = startupInventoryCounts{}
			runDirs, inventoryErr = startupRecoveryRunDirs(ctx, l, nil, observe)
		}
		return inventoryErr
	})
	// Per-source counts, on the phase's own line: which source the candidate
	// set came from, and how much of it is durable markers rather than
	// projected non-terminal runs.
	pf(stdout, "%s startup phase=recovery-run-inventory status=tally source=%s %s\n", startupTimestamp(), source, counts)
	tracker.setRecoveryAccumulation(len(runDirs))
	budget := tracker.budgetSnapshot(time.Now())
	pf(stdout, "%s startup budget: worktrees=%d recovery-runs=%d total=%d budget=%s state=%s used=%.1f%%\n",
		startupTimestamp(), budget.Accumulation.Worktrees, budget.Accumulation.RecoveryRuns,
		budget.Accumulation.total(), budget.Budget, budget.State, budget.UsedPercent)
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

// crashResumeProgressInterval bounds how often the crash-resume phase reports
// its position. Every other startup phase logs a start and a done line; this
// one can run for the better part of an hour between them, and on the live
// instance it emitted nothing at all for 51 minutes while the whole instance
// scheduled no work (#5199). One line per interval is enough to tell a slow
// pass from a hung one, and few enough not to flood a restart log.
const crashResumeProgressInterval = 30 * time.Second

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
) (outcome resumeOutcome, err error) {
	target := fmt.Sprintf("candidates=%d", len(recoveryRunDirs))
	progress := newStartupProgressReporter(stdout, tracker, "crash-resume", target, crashResumeProgressInterval)
	err = runStartupPhase(stdout, tracker, "crash-resume", target, func() error {
		var resumeErr error
		outcome, resumeErr = resumeInterruptedRunsWithRunners(
			ctx, l, setup.Runners, setup.LegacyRunner, setup.RunnerRegistry, guards,
			setup.Machines, setup.GooberDigests, setup.RepoRefs, setup.InstanceLog,
			setup.Telemetry, setup.RollupDB, setup.Watermarks, sched.ReleaseReconciled,
			wg, func(seen resumeOutcome) { progress.report(seen.Summary()) }, recoveryRunDirs,
		)
		return resumeErr
	})
	// The tallies, always — a phase that resumed nothing at all is exactly
	// the case the startup log has to be able to state rather than imply.
	pf(stdout, "%s startup phase=crash-resume status=tally target=%q %s\n", startupTimestamp(), target, outcome.Summary())
	return outcome, err
}

// startStartupTerminalFinalize completes the terminal half of crash-resume in
// the background, AFTER readiness. See terminalFinalization: none of this
// gates scheduling, and on the live instance all 1,481 candidates were of this
// kind, costing a 51-minute scheduling outage on every restart.
//
// It reports through the instance journal, never stdout: this goroutine runs
// concurrently with the daemon loop's own writes, and an io.Writer is not
// required to be safe for that (the same rule every other background sweep in
// up.go follows). The crash-resume tally line already named how many terminal
// candidates were handed over.
//
// Whatever it does not reach before shutdown is finished by
// finishAfterDrain, so deferring this work never drops it.
func startStartupTerminalFinalize(ctx context.Context, setup *schedulerSetup, candidates []terminalFinalization) *startupTerminalFinalizer {
	finalizer := &startupTerminalFinalizer{
		remaining: candidates, log: setup.InstanceLog, watermarks: setup.Watermarks,
		reporter: newSweepErrorReporter(setup.InstanceLog, "startup_terminal_finalize_failed"),
		done:     make(chan struct{}),
	}
	go func() {
		defer close(finalizer.done)
		err := finalizer.run(ctx, nil)
		if ctx.Err() == nil {
			finalizer.reporter.report(err)
		}
	}()
	return finalizer
}

// startupProgressReporter rate-limits progress lines for one startup phase and
// keeps the phase tracker's target current, so the readiness diagnostic names
// the same position the log does.
type startupProgressReporter struct {
	stdout   io.Writer
	tracker  *startupPhaseTracker
	phase    string
	target   string
	interval time.Duration
	last     time.Time
}

func newStartupProgressReporter(stdout io.Writer, tracker *startupPhaseTracker, phase, target string, interval time.Duration) *startupProgressReporter {
	return &startupProgressReporter{stdout: stdout, tracker: tracker, phase: phase, target: target, interval: interval, last: time.Now()}
}

// report is called from the phase's own goroutine only, so it needs no lock of
// its own — the tracker it updates has one.
func (r *startupProgressReporter) report(position string) {
	if r.tracker != nil {
		r.tracker.update(r.phase, r.target+" "+position)
	}
	if time.Since(r.last) < r.interval {
		return
	}
	r.last = time.Now()
	pf(r.stdout, "%s startup phase=%s status=progress target=%q %s\n", startupTimestamp(), r.phase, r.target, position)
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
	progress func(startupInventoryCounts),
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
			progress(startupInventoryCounts{Marked: len(marked), Discovered: len(runDirs) - len(marked), More: page.HasMore})
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
	progress func(startupInventoryCounts),
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
			progress(startupInventoryCounts{Discovered: len(runDirs), More: rootIndex+1 < len(roots)})
		}
	}
	return runDirs, nil
}
