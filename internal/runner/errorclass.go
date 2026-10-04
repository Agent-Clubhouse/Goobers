package runner

import (
	"errors"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/worktree"
)

// Typed dispatch-failure codes the runner authors itself. Codes an executor or
// provider already reports (github_auth_failed, claims_lock_timeout, timeout,
// …) pass through untouched — this set only names the causes the runner is the
// first to know about.
const (
	// errCodeInfraGit is a git-reported provisioning failure that is not a
	// transport failure: an unauthorized clone, a missing ref, a credential
	// helper that will not exec.
	errCodeInfraGit = "infra_git_failed"
	// errCodeInfraNet is a failure to reach the remote at all (DNS,
	// connectivity, transport timeout, remote 5xx).
	errCodeInfraNet = "infra_net_failed"
	// errCodeInfraWorkspace is a workspace-provisioning failure git never
	// reported — scratch-dir creation, a pinned-lease conflict, a
	// filesystem error.
	errCodeInfraWorkspace = "infra_workspace_failed"
	// errCodeExecutor is the residual: a dispatch failure no typed error
	// explained, which after this classification means a genuine runner or
	// executor defect rather than "something went wrong somewhere".
	errCodeExecutor = "executor_error"
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

// Runner-namespace keys carrying a dispatch failure's typed cause on its
// error event. The journal's normative Error.Code stays executor_error for
// every dispatch failure — that exact string is how three attempt-boundary
// projections outside this package recognize an attempt that failed before it
// could report a result — so the refinement lives in the runner namespace,
// which is excluded from conformance by construction and is what
// internal/telemetry/rollup projects into stage_attempts.
const (
	stageErrorCodeKey  = "errorCode"
	stageErrorClassKey = "errorClass"
)

// stageCodedError is the structural seam for a failure that already knows its
// own machine-readable code — satisfied by internal/executor.StageError and by
// codedStageError below. Matching an interface rather than a concrete type
// keeps the runner core dependent on the invoke seam alone, never on a
// particular executor implementation.
type stageCodedError interface {
	error
	StageErrorCode() string
}

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

// codedStageFailure tags err with code, preserving its message verbatim.
func codedStageFailure(code string, err error) error {
	if err == nil || code == "" {
		return err
	}
	return &codedStageError{code: code, err: err}
}

// DeclaredArtifactRetryFailure converts agent-authored declared-artifact
// contract failures back into a policy-class dispatch failure so a stage's
// retry.maxAttempts budget covers transient omitted or malformed artifacts.
// An UNCOMMITTED_CHANGES result (#5182) is the same kind of agent-side
// completion-contract miss and takes the same retry path.
func DeclaredArtifactRetryFailure(result apiv1.ResultEnvelope) error {
	if result.Status != apiv1.ResultFailure || result.Error == nil || !result.Error.Retryable {
		return nil
	}
	switch result.Error.Code {
	case missingDeclaredArtifactCode, invalidDeclaredArtifactSet, UncommittedChangesCode:
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
	return codedStageFailure(result.Error.Code, errors.New(message))
}

// declaredArtifactRetryError turns a clean dispatch that ended in a retryable
// declared-artifact failure into a policy-class dispatch error, but only while
// the stage still has a policy attempt left. The final attempt keeps its
// ordinary failure result so stage.finished, continueOnError and failure
// routing see it exactly as they did before retries covered it (#5560).
func declaredArtifactRetryError(dispatchErr error, result apiv1.ResultEnvelope, policyRetryRemains bool) error {
	if dispatchErr != nil || !policyRetryRemains {
		return dispatchErr
	}
	return DeclaredArtifactRetryFailure(result)
}

// classifyDispatchFailure resolves the typed error code and class the runner
// journals for a failed stage dispatch. Before it existed every one of these
// failures — an unauthorized clone, a broken askpass helper, a DNS outage, a
// claims-lock timeout, a rate-limited provider retry — reached telemetry as
// error_code=executor_error / error_class=unknown, recoverable only by
// reading each run's journal text by hand (2026-08-08 reliability audit: 87%
// of one gaggle's 3,987 stage failures were that single bucket).
//
// Classification is structural throughout: a typed code carried by the error
// itself wins, then the invoke seam's own timeout marker, then the residual
// executor bug. Nothing here matches on message text.
func classifyDispatchFailure(err error) (string, telemetry.ErrorClass) {
	code := errCodeExecutor
	var coded stageCodedError
	switch {
	case errors.As(err, &coded) && coded.StageErrorCode() != "":
		code = coded.StageErrorCode()
	case invoke.IsTimeout(err):
		code = telemetry.ErrCodeTimeout
	}
	return code, telemetry.ClassifyError(code)
}

// provisionFailureCode names why a stage's workspace could not be provisioned,
// separating the three owners git's uniform exit 128 hides: the credential
// (infra_git), the network (infra_net), and the host (infra_workspace).
func provisionFailureCode(err error) string {
	var coded stageCodedError
	if errors.As(err, &coded) && coded.StageErrorCode() != "" {
		return coded.StageErrorCode()
	}
	switch worktree.ClassifyProvisionError(err) {
	case worktree.TierNetwork:
		return errCodeInfraNet
	case worktree.TierGit:
		return errCodeInfraGit
	default:
		return errCodeInfraWorkspace
	}
}
