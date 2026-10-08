package childworkflow

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// HTTPService verifies the stage credential independently of the router. This
// also authenticates local-loopback deployments without human authentication.
// Submission repeats current authority checks and transactional grant fencing.
type HTTPService struct {
	Submission *SubmissionService
	Grants     *podauth.SignedKey
}

var _ httpapi.ChildWorkflowService = (*HTTPService)(nil)

func (s *HTTPService) authenticate(token, run string) (Origin, error) {
	if s.Submission == nil || s.Grants == nil {
		return Origin{}, childHTTPError(http.StatusServiceUnavailable, "child_workflows_unavailable", "child workflows are unavailable", nil)
	}
	grant, err := s.Grants.VerifyChildWorkflowGrant(token)
	if err != nil {
		return Origin{}, childHTTPError(http.StatusUnauthorized, "child_workflow_grant_invalid", "a valid unexpired child-workflow stage grant is required", nil)
	}
	if grant.RunID != run {
		return Origin{}, childHTTPError(http.StatusForbidden, "child_workflow_wrong_parent", "the stage grant does not authorize this parent", nil)
	}
	return Origin{GrantID: grant.ID, Gaggle: grant.Gaggle, RunID: grant.RunID, StageOccurrence: grant.StageOccurrence,
		AttemptID: grant.AttemptID, ConfigDigest: grant.ConfigDigest, PolicyDigest: grant.PolicyDigest}, nil
}

// ValidateChildWorkflow returns advisory findings without storing a proposal.
func (s *HTTPService) ValidateChildWorkflow(ctx context.Context, token, run string, source []byte) (apicontract.ChildWorkflowValidationResponse, error) {
	result := apicontract.ChildWorkflowValidationResponse{Advisory: true, Diagnostics: []apicontract.ChildWorkflowDiagnostic{}}
	origin, err := s.authenticate(token, run)
	if err != nil {
		return result, err
	}
	proposal, err := s.Submission.Validate(ctx, origin, source)
	var invalid *ValidationError
	if errors.As(err, &invalid) {
		for _, diagnostic := range invalid.Diagnostics {
			result.Diagnostics = append(result.Diagnostics, apicontract.ChildWorkflowDiagnostic{
				Code: diagnostic.Code, Stage: diagnostic.Stage, Field: diagnostic.Field, Message: diagnostic.Message,
			})
		}
		return result, nil
	}
	if err != nil {
		return result, childOperationError(err)
	}
	result.Valid = true
	result.SourceDigest, result.CanonicalDigest = proposal.SourceDigest, proposal.CanonicalDigest
	result.ConfigDigest, result.PolicyDigest, result.WorkflowDigest = proposal.ConfigDigest, proposal.PolicyDigest, proposal.Machine.Digest()
	return result, nil
}

// StartChildWorkflow acknowledges custody only after the queue transaction commits.
func (s *HTTPService) StartChildWorkflow(ctx context.Context, token, run, invocationKey string, source []byte) (apicontract.ChildWorkflowResponse, error) {
	origin, err := s.authenticate(token, run)
	if err != nil {
		return apicontract.ChildWorkflowResponse{}, err
	}
	submission, err := s.Submission.Submit(ctx, origin, SubmissionRequest{InvocationKey: invocationKey, Source: source})
	if err != nil {
		return apicontract.ChildWorkflowResponse{}, childOperationError(err)
	}
	return childResponse(submission), nil
}

// ChildWorkflowStatus reads only the authenticated occurrence's invocation.
func (s *HTTPService) ChildWorkflowStatus(ctx context.Context, token, run, invocationKey string) (apicontract.ChildWorkflowResponse, error) {
	origin, err := s.authenticate(token, run)
	if err != nil {
		return apicontract.ChildWorkflowResponse{}, err
	}
	submission, err := s.Submission.Get(ctx, origin, invocationKey)
	if err != nil {
		return apicontract.ChildWorkflowResponse{}, childOperationError(err)
	}
	return childResponse(submission), nil
}

func childResponse(submission Submission) apicontract.ChildWorkflowResponse {
	c, e := submission.Child, submission.Envelope
	return apicontract.ChildWorkflowResponse{
		ChildID: c.ChildID, AcceptanceID: c.AcceptanceID, RunID: c.RunID, InvocationKey: c.Identity.InvocationKey,
		Sequence: c.Sequence, State: string(c.State), Duplicate: submission.Duplicate,
		SourceDigest: e.SourceDigest, CanonicalDigest: e.CanonicalDigest, ConfigDigest: e.ConfigDigest,
		PolicyDigest: e.PolicyDigest, WorkflowDigest: e.WorkflowDigest, CancellationRequested: c.CancellationRequested,
		ResultRef: c.ResultRef, WorkspaceRef: c.WorkspaceRef, AcceptedAt: c.AcceptedAt, UpdatedAt: c.UpdatedAt,
	}
}

func childHTTPError(status int, code, message string, cause error) error {
	return httpapi.NewInterventionError(status, code, message, cause)
}

func childOperationError(err error) error {
	var invalid *ValidationError
	switch {
	case errors.Is(err, ErrAuthorityUnavailable), errors.Is(err, ErrAuthorityChanged), errors.Is(err, triggerqueue.ErrChildAuthorityChanged):
		return childHTTPError(http.StatusForbidden, "child_workflow_authority_changed", "the stage no longer holds current child-workflow authority", nil)
	case errors.As(err, &invalid), errors.Is(err, ErrSubmissionInvalid):
		return childHTTPError(http.StatusUnprocessableEntity, "child_workflow_invalid", "the child proposal or invocation is invalid; validate the proposal before starting it", nil)
	case errors.Is(err, triggerqueue.ErrConflict):
		return childHTTPError(http.StatusConflict, "child_workflow_conflict", "the invocation key already identifies different child content", nil)
	case errors.Is(err, triggerqueue.ErrChildSlotOccupied):
		return childHTTPError(http.StatusConflict, "child_workflow_unresolved", "this stage occurrence has an unresolved child", nil)
	case errors.Is(err, triggerqueue.ErrChildLimit):
		return childHTTPError(http.StatusConflict, "child_workflow_limit", "this stage occurrence has reached its child allowance", nil)
	case errors.Is(err, triggerqueue.ErrParentCancelled), errors.Is(err, triggerqueue.ErrParentSettled):
		return childHTTPError(http.StatusConflict, "child_workflow_parent_closed", "the parent can no longer start children", nil)
	case errors.Is(err, sql.ErrNoRows):
		return childHTTPError(http.StatusNotFound, "child_workflow_not_found", "no child is retained for this invocation", nil)
	case errors.Is(err, ErrProposalExpired):
		return childHTTPError(http.StatusGone, "child_workflow_expired", "the retained child proposal has expired", nil)
	case errors.Is(err, triggerqueue.ErrFull):
		return childHTTPError(http.StatusServiceUnavailable, "child_workflow_capacity", "child acceptance capacity is unavailable; retry with the same invocation key", nil)
	case errors.Is(err, triggerqueue.ErrChildProposalUnavailable):
		return childHTTPError(http.StatusServiceUnavailable, "child_workflow_custody_unavailable", "retained child evidence requires recovery", nil)
	default:
		return childHTTPError(http.StatusServiceUnavailable, "child_workflow_unavailable", "child operation could not be confirmed; retry with the same invocation key", err)
	}
}
