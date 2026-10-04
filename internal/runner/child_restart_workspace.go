package runner

import (
	"encoding/json"
	"errors"
	"maps"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// BindChildRestartWorkspace pins the trusted fork prepared from the sealed
// prior result. Call only after queue epoch admission; the durable plan itself
// remains independent of these derived custody selectors.
func BindChildRestartWorkspace(plan StageRestartPlan, admission *ChildWorkspaceAdmission) (StageRestartPlan, error) {
	req := &plan.Continuation
	if plan.Source.Child == nil || req.ChildContinuation == nil || req.ChildContinuation.ExecutionEpoch != plan.Source.Child.ExecutionEpoch+1 {
		return StageRestartPlan{}, errors.New("runner: child restart workspace requires admitted epoch lineage")
	}
	if admission == nil {
		if strings.HasPrefix(plan.Source.WorkspaceBranch, "goobers/children/") || req.Inputs[ChildWorkspaceInputName] != nil {
			return StageRestartPlan{}, errors.New("runner: child repository restart requires retained fork custody")
		}
		return plan, nil
	}
	if err := admission.validate(); err != nil {
		return StageRestartPlan{}, err
	}
	if admission.WorkspaceID != req.RunID+"-child" || plan.Source.WorkspaceRepository == nil {
		return StageRestartPlan{}, errors.New("runner: child restart fork differs from epoch or source repository")
	}
	raw, err := json.Marshal(admission)
	if err != nil {
		return StageRestartPlan{}, err
	}
	req.Inputs = maps.Clone(req.Inputs)
	req.InputIntegrity = maps.Clone(req.InputIntegrity)
	req.InputSource = maps.Clone(req.InputSource)
	if req.Inputs == nil {
		req.Inputs = map[string][]byte{}
	}
	if req.InputIntegrity == nil {
		req.InputIntegrity = map[string]apiv1.Integrity{}
	}
	if req.InputSource == nil {
		req.InputSource = map[string]string{}
	}
	req.Inputs[ChildWorkspaceInputName] = raw
	req.InputIntegrity[ChildWorkspaceInputName] = apiv1.IntegrityTrusted
	req.InputSource[ChildWorkspaceInputName] = "child-epoch-custody"
	req.ChildWorkspace = &journal.ChildContinuationWorkspace{Branch: "goobers/children/" + req.RunID, ForkSHA: admission.ForkSHA}
	return plan, nil
}
