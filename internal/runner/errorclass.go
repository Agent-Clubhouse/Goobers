package runner

import (
	"errors"

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
	switch worktree.ClassifyProvisionError(err) {
	case worktree.TierNetwork:
		return errCodeInfraNet
	case worktree.TierGit:
		return errCodeInfraGit
	default:
		return errCodeInfraWorkspace
	}
}
