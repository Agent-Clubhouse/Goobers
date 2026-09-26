// Package workspacerevision implements immutable workspace-revision authority
// checks and repository policy resolution for the selected-revision contract.
package workspacerevision

import (
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	// CodeInvalid reports a malformed or structurally invalid selected revision.
	CodeInvalid = apiv1.WorkspaceRevisionInvalidCode
	// CodeUnauthorized reports a selected revision not allowed by configured repos.
	CodeUnauthorized = "workspace_revision_unauthorized"
	// CodeConflict reports a conflicting revision after authority was established.
	CodeConflict = "workspace_revision_conflict"
	// CodeAcquisition reports a transient acquisition failure for the selected revision.
	CodeAcquisition = "workspace_revision_acquisition"
	// CodeObjectType reports an unsupported object type on the selected revision.
	CodeObjectType = "workspace_revision_object_type"
	// CodeSHAMismatch reports a revision mismatch against the configured base/expected object.
	CodeSHAMismatch = "workspace_revision_sha_mismatch"
)

// Error represents an invalid or unauthorized selected-revision decision.
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

// Unwrap returns the underlying cause of the selected-revision error.
func (e *Error) Unwrap() error { return e.Cause }

// NonRetryable reports whether the selected-revision error should not be retried.
func (e *Error) NonRetryable() bool { return e.Code != CodeAcquisition }

// StageErrorCode returns the canonical stage error code for the selected revision.
func (e *Error) StageErrorCode() string { return e.Code }

// Accept preserves immutable authority after the caller verifies eligibility
// and configuration authorization.
func Accept(current, candidate *apiv1.WorkspaceRevision) (*apiv1.WorkspaceRevision, error) {
	if candidate == nil {
		return current, nil
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
