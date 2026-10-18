package main

import (
	"context"
	"os"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/readservice"
)

// daemonInstanceReadinessService backs the readiness-gate endpoint (#5019):
// durable root identity read fresh off disk each call (cheap — the daemon is
// the only writer), plus the same startup phase tracker (#4368) and Ready
// gate (#3806) that already back the startup-diagnostic log line and
// /readyz.
//
// It is wired into the router before crash-orphan Reap runs and is the one
// route the recovery gate in httpapi.Router.serve never blocks, so every
// field it reports must stay safe to read the instant the daemon has a
// Layout and a loaded config — well before the read model, active-run
// counts, or any of the other subsystems /api/v1/instance depends on exist.
type daemonInstanceReadinessService struct {
	instanceRoot string
	tracker      *startupPhaseTracker
	ready        func() bool
}

func (s *daemonInstanceReadinessService) InstanceReadiness(context.Context) (httpapi.InstanceReadiness, error) {
	computerName, _ := os.Hostname()
	phase, target, since := s.tracker.snapshot()
	var elapsed time.Duration
	if !since.IsZero() {
		elapsed = time.Since(since)
	}
	budget := s.tracker.budgetSnapshot(time.Now())
	candidate := httpRecoveryCandidate(s.tracker.recoverySnapshot(), time.Now(), true)
	return httpapi.InstanceReadiness{
		APIVersion:    readservice.APIVersion,
		SchemaVersion: readservice.SchemaVersion,
		ComputerName:  computerName,
		InstanceRoot:  s.instanceRoot,
		RootIdentity:  readservice.InspectRootIdentity(s.instanceRoot),
		Ready:         s.ready(),
		Recovery: httpapi.InstanceRecoveryPhase{
			Phase:             phase,
			Target:            target,
			ElapsedSeconds:    elapsed.Seconds(),
			WorktreeCount:     budget.Accumulation.Worktrees,
			RecoveryRunCount:  budget.Accumulation.RecoveryRuns,
			AccumulationCount: budget.Accumulation.total(),
			BudgetSeconds:     budget.Budget.Seconds(),
			BudgetUsedPercent: budget.UsedPercent,
			BudgetState:       budget.State,
			BlockingCandidate: candidate,
		},
	}, nil
}

func httpRecoveryCandidate(candidate *startupRecoveryCandidate, now time.Time, includeIdentity bool) *httpapi.RecoveryCandidateStatus {
	if candidate == nil {
		return nil
	}
	status := &httpapi.RecoveryCandidateStatus{
		Progress: httpapi.RecoveryProgress{
			Total:      candidate.Total,
			Examined:   candidate.Examined,
			Resumed:    candidate.Resumed,
			Reattached: candidate.Reattached,
			Terminal:   candidate.Terminal,
			Skipped:    candidate.Skipped,
		},
		Disposition:    candidate.Disposition,
		Phase:          candidate.Phase,
		Operation:      candidate.Operation,
		StartedAt:      candidate.StartedAt,
		LastProgressAt: candidate.LastProgressAt,
	}
	if includeIdentity {
		status.RunID = candidate.RunID
		status.Gaggle = candidate.Gaggle
		status.Workflow = candidate.Workflow
	}
	if !candidate.StartedAt.IsZero() {
		status.ElapsedSeconds = now.Sub(candidate.StartedAt).Seconds()
	}
	if !candidate.LastProgressAt.IsZero() {
		status.ProgressAgeSecs = now.Sub(candidate.LastProgressAt).Seconds()
	}
	return status
}

func readserviceRecoveryCandidate(candidate *startupRecoveryCandidate, now time.Time) *readservice.StartupRecoveryCandidate {
	if candidate == nil {
		return nil
	}
	status := &readservice.StartupRecoveryCandidate{
		Progress: readservice.StartupRecoveryProgress{
			Total:      candidate.Total,
			Examined:   candidate.Examined,
			Resumed:    candidate.Resumed,
			Reattached: candidate.Reattached,
			Terminal:   candidate.Terminal,
			Skipped:    candidate.Skipped,
		},
		RunID:          candidate.RunID,
		Gaggle:         candidate.Gaggle,
		Workflow:       candidate.Workflow,
		Disposition:    candidate.Disposition,
		Phase:          candidate.Phase,
		Operation:      candidate.Operation,
		StartedAt:      candidate.StartedAt,
		LastProgressAt: candidate.LastProgressAt,
	}
	if !candidate.StartedAt.IsZero() {
		status.ElapsedSeconds = now.Sub(candidate.StartedAt).Seconds()
	}
	if !candidate.LastProgressAt.IsZero() {
		status.ProgressAgeSecs = now.Sub(candidate.LastProgressAt).Seconds()
	}
	return status
}

func readserviceStartupStatus(tracker *startupPhaseTracker, ready bool) *readservice.StartupStatus {
	if ready || tracker == nil {
		return nil
	}
	phase, target, since := tracker.snapshot()
	if phase == "" {
		return nil
	}
	now := time.Now()
	budget := tracker.budgetSnapshot(now)
	elapsed := time.Duration(0)
	if !since.IsZero() {
		elapsed = now.Sub(since)
	}
	return &readservice.StartupStatus{
		Phase:             phase,
		Target:            target,
		Since:             since,
		ElapsedSeconds:    elapsed.Seconds(),
		WorktreeCount:     budget.Accumulation.Worktrees,
		RecoveryRunCount:  budget.Accumulation.RecoveryRuns,
		AccumulationCount: budget.Accumulation.total(),
		BudgetSeconds:     budget.Budget.Seconds(),
		BudgetUsedPercent: budget.UsedPercent,
		BudgetState:       budget.State,
		BlockingCandidate: readserviceRecoveryCandidate(tracker.recoverySnapshot(), now),
	}
}
