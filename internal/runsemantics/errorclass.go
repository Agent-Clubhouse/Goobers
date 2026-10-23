package runsemantics

import (
	"errors"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/telemetry"
)

const (
	missingDeclaredArtifactCode = "missing_declared_artifact"
	invalidDeclaredArtifactSet  = "invalid_declared_artifact_set"
)

// UncommittedChangesCode is the harness's typed failure for a stage that
// declares modify-repository and reported success with its work left
// uncommitted (#5182; internal/harness.ErrorCodeUncommittedChanges). It
// retries on the stage's policy budget like a missing declared artifact: the
// stage is sent back to commit its own work, instead of reaching the reviewer
// gate as an empty diff that fails terminally.
const UncommittedChangesCode = "UNCOMMITTED_CHANGES"

// PrematureExhaustionClaimCode is the harness's typed failure for an agent
// that reported its execution window exhausted well before the runtime
// deadline (#5871; internal/harness.ErrorCodePrematureExhaustionClaim). The
// agent stopped early rather than timing out, so like the other agent-side
// completion-contract misses it retries on the stage's policy budget.
const PrematureExhaustionClaimCode = telemetry.ErrCodePrematureExhaustionClaim

// codedStageError is the runner's construction-site typing for a failure it
// classifies itself. Introduced so classification happens once, where the
// cause is still known, instead of downstream consumers re-deriving it from
// message text.
type codedStageError struct {
	code string
	err  error
}

func (e *codedStageError) Error() string { return e.err.Error() }

func (e *codedStageError) Unwrap() error { return e.err }

func (e *codedStageError) StageErrorCode() string { return e.code }

// CodedStageFailure tags err with code, preserving its message verbatim.
func CodedStageFailure(code string, err error) error {
	if err == nil || code == "" {
		return err
	}
	return &codedStageError{code: code, err: err}
}

// DeclaredArtifactRetryFailure converts agent-authored declared-artifact
// contract failures back into a policy-class dispatch failure so a stage's
// retry.maxAttempts budget covers transient omitted or malformed artifacts.
// An UNCOMMITTED_CHANGES result (#5182) and a PREMATURE_EXHAUSTION_CLAIM
// (#5871) are the same kind of agent-side completion-contract miss and take
// the same retry path.
func DeclaredArtifactRetryFailure(result apiv1.ResultEnvelope) error {
	if result.Status != apiv1.ResultFailure || result.Error == nil || !result.Error.Retryable {
		return nil
	}
	switch result.Error.Code {
	case missingDeclaredArtifactCode, invalidDeclaredArtifactSet, UncommittedChangesCode, PrematureExhaustionClaimCode:
	default:
		return nil
	}
	message := strings.TrimSpace(result.Error.Message)
	if message == "" {
		message = strings.TrimSpace(result.Summary)
	}
	if message == "" {
		message = result.Error.Code
	}
	return CodedStageFailure(result.Error.Code, errors.New(message))
}
