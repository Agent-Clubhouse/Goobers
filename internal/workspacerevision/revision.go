// Package workspacerevision implements substrate-independent selected-revision
// transitions and configuration authorization. It never resolves refs or acquires
// credentials; callers must use the configured reference returned by Resolve.
package workspacerevision

import (
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Stable selected-revision failure codes shared by every execution substrate.
const (
	CodeInvalid      = "workspace_revision_invalid"
	CodeUnauthorized = "workspace_revision_unauthorized"
	CodeConflict     = "workspace_revision_conflict"
	CodeAcquisition  = "workspace_revision_acquisition"
	CodeObjectType   = "workspace_revision_object_type"
	CodeSHAMismatch  = "workspace_revision_sha_mismatch"
)

// Error is a stable, non-retryable selected-revision refusal. Substrates may
// preserve Cause for diagnostics without changing the code on the wire.
type Error struct {
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

// Unwrap exposes acquisition or validation detail to errors.Is/errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// NonRetryable prevents retries from turning a refusal into branch fallback.
func (e *Error) NonRetryable() bool { return true }

// StageErrorCode preserves the classification through runner terminal failures.
func (e *Error) StageErrorCode() string { return e.Code }

// Accept establishes an independently owned immutable binding. A byte-for-byte
// equivalent value (including provenance) is idempotent. Missing legacy controls
// and unsuccessful deterministic results leave the binding unchanged.
func Accept(current, candidate *apiv1.WorkspaceRevision, deterministic, success bool) (*apiv1.WorkspaceRevision, error) {
	if candidate == nil {
		return current, nil
	}
	if !deterministic {
		return current, &Error{Code: CodeUnauthorized, Message: "agentic results cannot establish workspace revision authority"}
	}
	if !success {
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
