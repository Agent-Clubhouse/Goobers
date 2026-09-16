package main

import (
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/runner"
	telemetryingest "github.com/goobers/goobers/internal/telemetry/ingest"
	"github.com/goobers/goobers/internal/worktree"
)

// resumeOutcome is what one crash-resume pass found and did. It replaces the
// three parallel string slices the pass used to return, because #5199 needs
// the counts DURING the pass, not only after it: on the live instance the
// phase ran for 51 minutes emitting nothing at all, and an operator could not
// tell a slow resume from a hung one without reading /proc/1/io.
type resumeOutcome struct {
	// Total is the candidate set size; Examined advances through it.
	Total    int
	Examined int
	// Resumed, Warned and Reattached are run IDs, in the order encountered.
	Resumed    []string
	Warned     []string
	Reattached []string
	// Terminal are candidates that were already terminal. They are finalized
	// after readiness rather than ahead of it — see terminalFinalization.
	Terminal []terminalFinalization
}

// resumeProgressFunc observes one crash-resume pass as it advances. It is
// called once per candidate and once more when the pass ends; implementations
// throttle their own output.
type resumeProgressFunc func(resumeOutcome)

func (o resumeOutcome) report(progress resumeProgressFunc) {
	if progress != nil {
		progress(o)
	}
}

// Summary is the one-line tally an operator reads in the startup log.
func (o resumeOutcome) Summary() string {
	return fmt.Sprintf("examined=%d/%d resumed=%d reattached=%d terminal=%d skipped=%d",
		o.Examined, o.Total, len(o.Resumed), len(o.Reattached), len(o.Terminal), len(o.Warned))
}

// terminalFinalization is one already-terminal run found in the crash-resume
// candidate set. Its cleanup still has to happen — a worktree may be waiting
// on it, and its claims and read-model watermark with it — but nothing about
// it is a precondition for scheduling: the run is over, and its concurrency
// slot is released the moment it is classified (#5199).
//
// Keeping this off the critical path is what the phase costs measure: on the
// live instance, 1,481 candidates took 51 minutes and resumed exactly nothing
// — every one of them was terminal, and each paid a worktree FinalizeRun, a
// claim-ledger release and a full recovery-inventory read while the whole
// instance scheduled no work at all.
type terminalFinalization struct {
	layout   instance.Layout
	runsDir  string
	runner   *runner.Runner
	identity journal.RunIdentity
	phase    journal.RunPhase
}

// finalizeTerminalCandidates runs every collected terminal finalization,
// reporting progress as it goes. A cleanup that must be retried later is
// journaled, not fatal; any other failure is returned joined, so a caller on
// the startup path can still refuse to start and a caller after readiness can
// journal and carry on.
func finalizeTerminalCandidates(candidates []terminalFinalization, log *journal.InstanceLog, watermarks *intake.Store, progress func(done, total int)) error {
	var failures error
	for index, candidate := range candidates {
		if err := candidate.finalize(log); err != nil {
			failures = errors.Join(failures, err)
		}
		// #2190: a run that was already terminal at startup never took the
		// normal terminal run's telemetryingest.RunTelemetry path, so it
		// never recorded its intake watermark and the read model never
		// discovered it advanced.
		telemetryingest.RunIntake(watermarks, candidate.layout, candidate.identity.RunID, log)
		if progress != nil {
			progress(index+1, len(candidates))
		}
	}
	return failures
}

func (c terminalFinalization) finalize(log *journal.InstanceLog) error {
	var err error
	if c.runner != nil {
		err = c.runner.FinalizeTerminal(c.identity.RunID, c.phase)
	} else {
		manager, managerErr := worktree.NewManager(c.layout.WorkcopiesDir(), mutationCleanupGuard(c.runsDir))
		if managerErr != nil {
			err = managerErr
		} else {
			err = finalizeTerminalRun(c.layout, log, manager, c.identity.RunID)
		}
	}
	if err == nil {
		return nil
	}
	if !errors.Is(err, worktree.ErrCleanupDeferred) {
		return fmt.Errorf("finalize terminal run %q: %w", c.identity.RunID, err)
	}
	if log == nil {
		return nil
	}
	if appendErr := log.Append(journal.Event{
		Type: journal.EventError, Gaggle: c.identity.Gaggle, Workflow: c.identity.Workflow, RunID: c.identity.RunID,
		Error: &journal.ErrorDetail{
			Code:    "terminal_cleanup_deferred",
			Message: fmt.Sprintf("terminal cleanup deferred for retry: %v", err),
		},
	}); appendErr != nil {
		return fmt.Errorf("journal deferred terminal cleanup for run %q: %w", c.identity.RunID, appendErr)
	}
	return nil
}
