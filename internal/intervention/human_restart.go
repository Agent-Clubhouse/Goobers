package intervention

import (
	"context"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

// AttachStageRestarts installs the production credential-bound restart adapter.
// Without it, existing gate decisions and saved guidance remain available.
func (s *HumanService) AttachStageRestarts(service StageRestartService) {
	if service != nil {
		s.restarts.Store(&service)
	}
}

func validateRestartCommand(input apicontract.InteractiveRunCommand) error {
	if input.Guidance != "" || input.Decision != "" || strings.TrimSpace(input.Rationale) == "" || len(input.GuidanceIDs) < 1 || len(input.GuidanceIDs) > 16 {
		return interventionBadRequest("invalid_restart_command", "Restart requires a rationale and one to 16 saved guidance IDs.")
	}
	seen := map[string]bool{}
	for _, id := range input.GuidanceIDs {
		if id == "" || len(id) > 256 || seen[id] {
			return interventionBadRequest("invalid_restart_command", "Saved guidance IDs must be bounded and unique.")
		}
		seen[id] = true
	}
	return nil
}
func (s *HumanService) addRestartActions(p httpapi.Principal, resolved resolvedInterventionRun, view *apicontract.InteractiveRunView) {
	if s.restarts.Load() == nil {
		return
	}
	if resolved.phase != journal.PhaseEscalated && resolved.phase != journal.PhaseFailed {
		view.RestartReason = "Restart currently requires a settled failed or escalated local run. Paused runs can receive gate decisions and saved guidance."
		return
	}
	reader, err := journal.OpenReadOnly(resolved.runDir)
	if err != nil {
		return
	}
	id, err := reader.Identity()
	if err != nil {
		return
	}
	allowed := s.policy.Authorize(p, resolved.gaggle, "run.restartStage") == nil
	view.RestartReason = "Restart creates a linked execution with selected saved guidance and a fresh affected-stage allowance. Source history and run duration limits remain in force."
	for _, action := range append([]apicontract.InteractiveRunAction(nil), view.Actions...) {
		if action.Kind != "guidance" {
			continue
		}
		reason := ""
		supported := true
		if err := s.validateRestartTarget(id, resolved.machine, action.Stage); err != nil {
			reason = err.Error()
			supported = false
		}
		if !allowed {
			reason = "Your gaggle policy does not authorize stage restart."
		}
		view.Actions = append(view.Actions, apicontract.InteractiveRunAction{Kind: "restart", Stage: action.Stage, SubjectSequence: action.SubjectSequence, Decisions: []string{}, Available: allowed && supported, Reason: reason})
	}
}
func (s *HumanService) restartStage(admission, execution context.Context, p httpapi.Principal, resolved resolvedInterventionRun, key string, input apicontract.InteractiveRunCommand) (apicontract.InteractiveRunCommandResult, error) {
	adapter := s.restarts.Load()
	if adapter == nil {
		return apicontract.InteractiveRunCommandResult{}, interventionConflict("restart_unavailable", "Stage restart is unavailable on this daemon.")
	}
	if s.policy.Authorize(p, resolved.gaggle, "run.restartStage") != nil {
		return apicontract.InteractiveRunCommandResult{}, humanDenied()
	}
	reader, err := journal.OpenReadOnly(resolved.runDir)
	if err != nil {
		return apicontract.InteractiveRunCommandResult{}, err
	}
	// The key selects an epoch independently of the payload: changing a request
	// after uncertain acknowledgement conflicts with its retained manifest.
	epoch, err := restartEpochForCommand(filepath.Dir(resolved.runDir), resolved.runID, scopedHumanKey(p, resolved.gaggle, key))
	if err != nil {
		return apicontract.InteractiveRunCommandResult{}, err
	}
	prepare := runner.PrepareStageRestart
	if s.childRestartSupported() {
		prepare = runner.PrepareChildStageRestart
		id, readErr := reader.Identity()
		if readErr != nil {
			return apicontract.InteractiveRunCommandResult{}, readErr
		}
		if id.Child == nil {
			prepare = runner.PrepareStageRestart
		}
	}
	plan, err := prepare(reader, resolved.machine, runner.StageRestartRequest{EpochID: epoch, Stage: input.Stage, PrincipalRef: principalIdentity(p), ExpectedTerminalSeq: input.ExpectedSubjectSequence, GuidanceIDs: input.GuidanceIDs, Rationale: input.Rationale}, s.scrubber)
	if err != nil {
		return apicontract.InteractiveRunCommandResult{}, interventionConflict("restart_refused", err.Error())
	}
	accepted, err := (*adapter).RestartStage(admission, execution, p, plan)
	if err != nil {
		return apicontract.InteractiveRunCommandResult{}, err
	}
	if !apiv1.ValidRunID(accepted.RunID) || accepted.RunID != epoch {
		return apicontract.InteractiveRunCommandResult{}, interventionConflict("restart_receipt_invalid", "The restart receipt did not identify its reserved epoch.")
	}
	status := "started"
	if accepted.Queued {
		status = "pending"
	}
	return apicontract.InteractiveRunCommandResult{Status: status, Accepted: true, RunID: resolved.runID, ContinuationRunID: accepted.RunID, JournalSequence: input.ExpectedSubjectSequence, Phase: string(resolved.phase)}, nil
}

func (s *HumanService) childRestartSupported() bool {
	adapter := s.restarts.Load()
	if adapter == nil {
		return false
	}
	child, ok := (*adapter).(ChildStageRestartService)
	return ok && child.SupportsChildStageRestart()
}
func (s *HumanService) validateRestartTarget(id journal.RunIdentity, machine *workflow.Machine, stage string) error {
	if id.Child != nil && s.childRestartSupported() {
		return runner.ValidateChildStageRestartTarget(id, machine, stage)
	}
	return runner.ValidateStageRestartTarget(id, machine, stage)
}
