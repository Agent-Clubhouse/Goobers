package workspacerevision

import (
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// NormalizeResult validates before producer and status filtering. It never
// authorizes a repository or changes the accepted run binding.
func NormalizeResult(result *apiv1.ResultEnvelope, deterministic bool) *Error {
	if result.WorkspaceRevision == nil {
		return ResultRejection(*result)
	}
	if err := result.WorkspaceRevision.Validate(); err != nil {
		return &Error{Code: CodeInvalid, Message: err.Error(), Cause: err}
	}
	if !deterministic {
		return &Error{Code: CodeUnauthorized, Message: "agentic results cannot establish workspace revision authority"}
	}
	if result.Status != apiv1.ResultSuccess {
		result.WorkspaceRevision = nil
	}
	return nil
}

// NormalizeDeterministicResult retains command diagnostics when malformed
// authority replaces the command's reported outcome.
func NormalizeDeterministicResult(result apiv1.ResultEnvelope) apiv1.ResultEnvelope {
	if result.WorkspaceRevision == nil {
		return result
	}
	if err := NormalizeResult(&result, true); err != nil {
		result.Status = apiv1.ResultFailure
		result.WorkspaceRevision = nil
		result.Error = &apiv1.ErrorInfo{Code: err.Code, Message: err.Error()}
		result.Summary = "declared result contains an invalid workspace revision"
	}
	return result
}

// ResultRejection preserves canonical refusals transported as failure envelopes,
// after the command boundary has removed the invalid control.
func ResultRejection(result apiv1.ResultEnvelope) *Error {
	if result.Status != apiv1.ResultFailure || result.Error == nil {
		return nil
	}
	switch result.Error.Code {
	case CodeInvalid, CodeUnauthorized, CodeConflict, CodeObjectType, CodeSHAMismatch:
		return &Error{Code: result.Error.Code, Message: result.Error.Message}
	default:
		return nil
	}
}

// FromError recovers revision errors without reclassifying transport failures.
func FromError(err error) *Error {
	var revisionErr *Error
	if errors.As(err, &revisionErr) {
		return revisionErr
	}
	var decodeErr *apiv1.WorkspaceRevisionDecodeError
	if errors.As(err, &decodeErr) {
		return &Error{Code: CodeInvalid, Message: decodeErr.Error(), Cause: err}
	}
	return nil
}
