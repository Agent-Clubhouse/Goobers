package childengine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/objstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// FencedChildWorkflows serves the child-workflow routes (validate, start,
// status, resolve) to engine parents' stage pods from the replica's live
// controller lease epoch (#8085 c). Each request binds fresh custody and the
// parent authority, and childworkflow.SubmissionService resolves the parent's
// authority (run pins, active journal attempt, live engine execution) and
// checks the bound grant row on every operation. So a grant whose attempt
// ended, whose binding was revoked or replaced, or that control never bound
// authorizes nothing, whatever its signature (docs/design/
// engine-parent-child-authority.md §5).
type FencedChildWorkflows struct {
	Authority FencedAuthority
}

var (
	_ httpapi.ChildWorkflowService           = FencedChildWorkflows{}
	_ httpapi.ChildWorkflowResolutionService = FencedChildWorkflows{}
)

func (c FencedChildWorkflows) service(ctx context.Context) (*childworkflow.HTTPService, error) {
	store, source, parents, err := c.Authority.bound(ctx)
	if err != nil {
		return nil, httpapi.NewInterventionError(http.StatusServiceUnavailable, "child_workflows_unavailable",
			"child workflows are unavailable from this control plane; retry", err)
	}
	return childWorkflowService(store, source, parents, c.Authority.Grants, c.Authority.Fenced.Now), nil
}

// childWorkflowService is the one composition of the child-workflow service
// over bound custody, generation pins and the parent authority.
func childWorkflowService(store childworkflow.ChildQueue, pins generationPins, parents ParentAuthority, grants *podauth.SignedKey, now func() time.Time) *childworkflow.HTTPService {
	submission := &childworkflow.SubmissionService{Queue: pinningQueue{ChildQueue: store, pins: pins}, Authority: retryableAuthority{parents}, Now: now}
	return &childworkflow.HTTPService{Submission: submission, Grants: grants}
}

// ValidateChildWorkflow returns advisory findings without storing a proposal.
func (c FencedChildWorkflows) ValidateChildWorkflow(ctx context.Context, grant, run string, source []byte) (apicontract.ChildWorkflowValidationResponse, error) {
	service, err := c.service(ctx)
	if err != nil {
		return apicontract.ChildWorkflowValidationResponse{}, err
	}
	return service.ValidateChildWorkflow(ctx, grant, run, source)
}

// StartChildWorkflow accepts one child into fenced custody.
func (c FencedChildWorkflows) StartChildWorkflow(ctx context.Context, grant, run, invocationKey string, source []byte) (apicontract.ChildWorkflowResponse, error) {
	service, err := c.service(ctx)
	if err != nil {
		return apicontract.ChildWorkflowResponse{}, err
	}
	return service.StartChildWorkflow(ctx, grant, run, invocationKey, source)
}

// ChildWorkflowStatus reads the authenticated occurrence's invocation.
func (c FencedChildWorkflows) ChildWorkflowStatus(ctx context.Context, grant, run, invocationKey string) (apicontract.ChildWorkflowResponse, error) {
	service, err := c.service(ctx)
	if err != nil {
		return apicontract.ChildWorkflowResponse{}, err
	}
	return service.ChildWorkflowStatus(ctx, grant, run, invocationKey)
}

// ResolveChildWorkflow records the parent's choice for its terminal child.
func (c FencedChildWorkflows) ResolveChildWorkflow(ctx context.Context, grant, run string, request apicontract.ChildWorkflowResolveRequest) (apicontract.ChildWorkflowResolutionResponse, error) {
	service, err := c.service(ctx)
	if err != nil {
		return apicontract.ChildWorkflowResolutionResponse{}, err
	}
	return service.ResolveChildWorkflow(ctx, grant, run, request)
}

// retryableAuthority marks the parent-authority failures that are a brief
// loss of control, journal or engine as retryable, so the pod is told to
// retry rather than that its authority ended.
type retryableAuthority struct{ parents ParentAuthority }

func (r retryableAuthority) Resolve(ctx context.Context, origin childworkflow.Origin) (childworkflow.Authority, error) {
	authority, err := r.parents.Resolve(ctx, origin)
	if err != nil && errors.Is(classify(err), engine.ErrChildCustodyUnavailable) {
		return childworkflow.Authority{}, fmt.Errorf("%w: %w", childworkflow.ErrAuthorityRetryable, err)
	}
	return authority, err
}

// generationPins retains a child's pinned config generation for its owner.
type generationPins interface {
	PinGeneration(ctx context.Context, generation, owner string) error
}

var _ generationPins = (*objstore.Source)(nil)

// pinningQueue pins each accepted child's config generation, owned by its
// acceptance, so the generation its freeze recompiles from cannot be pruned
// before the engine binds the child; the bind releases it.
type pinningQueue struct {
	childworkflow.ChildQueue
	pins generationPins
}

func (q pinningQueue) AcceptChild(ctx context.Context, request triggerqueue.ChildAcceptance, now time.Time) (triggerqueue.ChildRecord, bool, error) {
	envelope, err := childworkflow.DecodeStartEnvelope(request.Payload)
	if err != nil {
		return triggerqueue.ChildRecord{}, false, err
	}
	child, duplicate, err := q.ChildQueue.AcceptChild(ctx, request, now)
	if err != nil {
		return child, duplicate, err
	}
	// Repeated for a duplicate: a pin lost to an earlier failed response is
	// restored, and re-pinning the same owner is idempotent.
	if err := q.pins.PinGeneration(ctx, envelope.ConfigGeneration, child.AcceptanceID); err != nil {
		return triggerqueue.ChildRecord{}, false, fmt.Errorf("%w: pin the child's config generation: %w", triggerqueue.ErrChildProposalUnavailable, err)
	}
	return child, duplicate, nil
}
