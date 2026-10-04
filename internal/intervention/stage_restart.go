package intervention

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
)

// StageRestartAcceptance identifies the one durable continuation for a command.
// Acceptance does not assert that an agent has already received its guidance.
type StageRestartAcceptance struct {
	RunID     string
	Duplicate bool
}

// StageRestartService admits a prepared human continuation using configured
// interactive credentials. Implementations fence policy through provider checks
// and bounded launch, never through the lifetime of resumed execution.
type StageRestartService interface {
	RestartStage(admission, execution context.Context, principal httpapi.Principal, plan runner.StageRestartPlan) (StageRestartAcceptance, error)
}

// LaunchStageRestart reserves normal execution capacity, reclaims the verified
// source claims, persists the immutable epoch and starts its pinned execution.
// The caller must hold current human policy authorization and must have checked
// provider eligibility/branch identity using explicit interactive credentials.
// A dedicated execution resolver is mandatory; the automation runner is never
// reused as an implicit fallback for a human continuation.
func (s *Service) LaunchStageRestart(admission, execution context.Context, plan runner.StageRestartPlan, preflight func(context.Context, *runner.StageRestartPlan) ([]localscheduler.ClaimEntry, error)) (StageRestartAcceptance, error) {
	if s.stageRestartExecution == nil {
		return StageRestartAcceptance{}, interventionConflict("restart_execution_unavailable", "Interactive restart credential wiring is unavailable.")
	}
	if err := admission.Err(); err != nil {
		return StageRestartAcceptance{}, err
	}
	if err := execution.Err(); err != nil {
		return StageRestartAcceptance{}, err
	}
	source, err := s.inspect(plan.Source.RunID)
	if err != nil {
		return StageRestartAcceptance{}, err
	}
	release, ok := s.trackActiveIntervention(source.runID)
	if !ok {
		return StageRestartAcceptance{}, interventionConflict("intervention_in_progress", "Another source intervention is active.")
	}
	defer release()
	if plan.Continuation.SourceRunID != source.runID || plan.Source.Gaggle != source.gaggle || plan.Source.WorkflowDigest != source.machine.Digest() || plan.Continuation.ExpectedTerminalSeq != source.terminalSeq {
		return StageRestartAcceptance{}, interventionConflict("restart_source_changed", "The source identity or terminal occurrence changed.")
	}
	dir := filepath.Join(filepath.Dir(source.runDir), plan.Continuation.RunID)
	duplicate, err := stageRestartReplay(dir, plan)
	if err != nil {
		return StageRestartAcceptance{}, err
	}
	if duplicate && (s.Active(plan.Continuation.RunID) || restartTerminal(dir)) {
		return StageRestartAcceptance{RunID: plan.Continuation.RunID, Duplicate: true}, nil
	}
	if preflight == nil {
		return StageRestartAcceptance{}, interventionConflict("restart_preflight_unavailable", "Provider and source admission is unavailable.")
	}
	verifiedClaims, err := preflight(admission, &plan)
	if err != nil {
		return StageRestartAcceptance{}, err
	}
	candidate, err := s.stageRestartExecution(admission, plan)
	if err != nil {
		return StageRestartAcceptance{}, err
	}
	if candidate.Runner == nil || candidate.Machine == nil || candidate.Machine.Digest() != plan.Source.WorkflowDigest || candidate.GooberDigest != plan.Source.GooberDigest {
		return StageRestartAcceptance{}, interventionConflict("restart_execution_changed", "Interactive execution differs from the retained workflow or goober pin.")
	}
	reserve, err := stageRestartReservation(admission, plan, candidate)
	if err != nil {
		return StageRestartAcceptance{}, err
	}
	target := resolvedInterventionRun{runID: plan.Continuation.RunID, runner: candidate.Runner, machine: candidate.Machine, gooberDigest: candidate.GooberDigest, repoRef: candidate.RepoRef, runDir: dir, gaggle: plan.Source.Gaggle, workflow: plan.Source.Workflow}
	lease, err := s.beginExecutionWithReservation(target, false, reserve)
	if err != nil {
		return StageRestartAcceptance{}, err
	}
	if err := s.persistRestartEpoch(admission, lease, plan, verifiedClaims, duplicate, candidate.ChildRestart); err != nil {
		return StageRestartAcceptance{}, err
	}
	s.executeStageRestart(execution, lease, candidate)
	return StageRestartAcceptance{RunID: target.runID, Duplicate: duplicate}, nil
}

func (s *Service) claimRestart(lease *interventionExecutionLease, claims []localscheduler.ClaimEntry) error {
	if s.claims == nil {
		return interventionConflict("restart_claims_unavailable", "Run claim admission is unavailable.")
	}
	acquired, holder, err := s.claims.Reclaim(claims, lease.resolved.gaggle, lease.resolved.runID, lease.resolved.workflow)
	if err != nil {
		return err
	}
	if !acquired {
		return interventionConflict("claim_unavailable", fmt.Sprintf("Source work is now held by run %q.", holder))
	}
	lease.reacquiredClaims = true
	return nil
}
func (s *Service) executeStageRestart(ctx context.Context, lease *interventionExecutionLease, candidate Execution) {
	if s.wg != nil {
		s.wg.Add(1)
	}
	go func() {
		if s.wg != nil {
			defer s.wg.Done()
		}
		result, err := s.finishExecution(ctx, lease, func(ctx context.Context) (runner.Result, error) {
			return candidate.Runner.Resume(ctx, restartResumeInput(ctx, lease, candidate))
		})
		if terminalInterventionPhase(result.Phase) {
			err = errors.Join(err, lease.releaseReacquiredClaims())
		}
		if err != nil && s.errorLog != nil {
			s.errorLog.Printf("human stage restart %s execution requires inspection: %v", lease.resolved.runID, err)
		}
	}()
}

func stageRestartReplay(dir string, plan runner.StageRestartPlan) (bool, error) {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return false, err
	}
	id, err := reader.Identity()
	if err != nil {
		return false, err
	}
	req := plan.Continuation
	if id.RunID != req.RunID || id.ContinuedFromRunID != req.SourceRunID || id.SourceTerminalSeq != req.ExpectedTerminalSeq || id.RequestedTarget != req.Target || id.Operator != req.Operator || id.WorkflowDigest != plan.Source.WorkflowDigest || id.GooberDigest != plan.Source.GooberDigest || id.Gaggle != plan.Source.Gaggle || len(id.Inputs) != len(req.Inputs) {
		return false, restartKeyConflict()
	}
	for _, input := range id.Inputs {
		raw, ok := req.Inputs[input.Name]
		if !ok || input.Ref.Digest != journal.Digest(raw) || input.Integrity != req.InputIntegrity[input.Name] || input.Source != req.InputSource[input.Name] {
			return false, restartKeyConflict()
		}
	}
	// The immutable snapshot carries the human request. Branch freshness is
	// checked only on first admission; later effects must not break receipt replay.
	if !reflect.DeepEqual(id.RunControls, plan.Source.RunControls) || !reflect.DeepEqual(id.Child, req.ChildContinuation) {
		return false, restartKeyConflict()
	}
	return true, nil
}
func restartKeyConflict() error {
	return httpapi.NewInterventionError(http.StatusConflict, "idempotency_key_reused", "This restart key already identifies a different continuation.", nil)
}

func restartTerminal(dir string) bool {
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return false
	}
	phase, err := reader.Phase()
	return err == nil && terminalInterventionPhase(phase)
}

func (s *Service) persistRestartEpoch(admission context.Context, lease *interventionExecutionLease, plan runner.StageRestartPlan, verifiedClaims []localscheduler.ClaimEntry, duplicate bool, child *ChildStageRestartAdmission) error {
	if child == nil {
		if err := s.claimRestart(lease, verifiedClaims); err != nil {
			lease.Close()
			return err
		}
	}
	if err := admission.Err(); err != nil {
		lease.Close()
		return errors.Join(err, lease.releaseReacquiredClaims())
	}
	if !duplicate {
		err := child.fence(admission, func() error {
			created, createErr := journal.CreateContinuation(filepath.Dir(lease.resolved.runDir), plan.Continuation)
			if createErr != nil {
				return createErr
			}
			if closeErr := created.Close(); closeErr != nil {
				lease.retainAdmission = true
				return closeErr
			}
			return nil
		})
		if err != nil {
			lease.Close()
			return errors.Join(err, lease.releaseReacquiredClaims())
		}
	}
	return nil
}
