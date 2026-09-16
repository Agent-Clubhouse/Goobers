package workspacebranch

import (
	"bytes"
	"encoding/json"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workspacerevision"
)

// ResultBinding promotes the reserved control, never a scalar output. Invalid
// ordinary result files retain the legacy parser's artifact-only behavior.
func ResultBinding(data []byte) (*apiv1.WorkspaceBranchBinding, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return nil, nil
	}
	raw, exists := fields["workspaceBranchBinding"]
	if !exists {
		return nil, nil
	}
	var binding *apiv1.WorkspaceBranchBinding
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil || binding == nil {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "invalid workspaceBranchBinding control", Cause: err}
	}
	if err := binding.Validate(); err != nil {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "invalid workspaceBranchBinding control", Cause: err}
	}
	return binding, nil
}

// ResultTip decodes the reserved acknowledgment separately from ordinary outputs.
func ResultTip(data []byte) (string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return "", nil
	}
	raw, exists := fields["workspaceBranchTip"]
	if !exists {
		return "", nil
	}
	var tip string
	if err := json.Unmarshal(raw, &tip); err != nil {
		return "", &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "invalid workspaceBranchTip control", Cause: err}
	}
	if err := apiv1.ValidateCommitSHA(tip); err != nil {
		return "", &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "invalid workspaceBranchTip control", Cause: err}
	}
	return tip, nil
}
