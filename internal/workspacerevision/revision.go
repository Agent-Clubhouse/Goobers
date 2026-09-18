package workspacerevision

import (
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	CodeInvalid      = "workspace_revision_invalid"
	CodeUnauthorized = "workspace_revision_unauthorized"
	CodeConflict     = "workspace_revision_conflict"
	CodeAcquisition  = "workspace_revision_acquisition"
	CodeObjectType   = "workspace_revision_object_type"
	CodeSHAMismatch  = "workspace_revision_sha_mismatch"
)

type Error struct {
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}
func (e *Error) Unwrap() error          { return e.Cause }
func (e *Error) NonRetryable() bool     { return e.Code != CodeAcquisition }
func (e *Error) StageErrorCode() string { return e.Code }

// Accept establishes immutable authority. Agentic, failed, and absent results
// cannot establish it; repeated identical deterministic values are idempotent.
func Accept(current, candidate *apiv1.WorkspaceRevision, deterministic, success bool) (*apiv1.WorkspaceRevision, error) {
	if candidate == nil || !success {
		return current, nil
	}
	if !deterministic {
		return current, &Error{Code: CodeUnauthorized, Message: "agentic results cannot establish workspace revision authority"}
	}
	if err := candidate.Validate(); err != nil {
		return current, &Error{Code: CodeInvalid, Message: err.Error(), Cause: err}
	}
	if current == nil {
		return candidate.DeepCopy(), nil
	}
	if !reflect.DeepEqual(current, candidate) {
		return current, &Error{Code: CodeConflict, Message: "workspace revision cannot change after establishment"}
	}
	return current, nil
}
