package childengine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
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
// custodyQueue is child custody that also reports the engine's bind.
// *acceptance.Store implements it.
type custodyQueue interface {
	childworkflow.ChildQueue
	ChildEngineStart(ctx context.Context, identity triggerqueue.ChildIdentity) (acceptance.ChildEngineStart, error)
}

func childWorkflowService(store custodyQueue, pins generationPins, parents ParentAuthority, grants *podauth.SignedKey, now func() time.Time) *childworkflow.HTTPService {
	submission := &childworkflow.SubmissionService{Queue: pinningQueue{custodyQueue: store, pins: pins}, Authority: retryableAuthority{parents}, Now: now}
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

// explicitRefusal reports a definite "no": the stage's authority ended,
// was replaced or revoked, or never existed. Anything else is a check that
// could not be made.
func explicitRefusal(err error) bool {
	class := classify(err)
	return errors.Is(class, engine.ErrChildCustodyRefused) && !errors.Is(class, engine.ErrChildCustodyUnavailable)
}

// retryableUnlessRefused marks every failure that is not an explicit refusal
// as retryable, so the pod is told its authority ended only when it did, and
// otherwise to retry.
func retryableUnlessRefused(err error) error {
	if err == nil || explicitRefusal(err) {
		return err
	}
	return fmt.Errorf("%w: %w", childworkflow.ErrAuthorityRetryable, err)
}

// retryableAuthority classifies the parent authority's failures.
type retryableAuthority struct{ parents ParentAuthority }

func (r retryableAuthority) Resolve(ctx context.Context, origin childworkflow.Origin) (childworkflow.Authority, error) {
	authority, err := r.parents.Resolve(ctx, origin)
	return authority, retryableUnlessRefused(err)
}

// generationPins retains a child's pinned config generation for its owner.
type generationPins interface {
	PinGeneration(ctx context.Context, generation, owner string) error
}

var _ generationPins = (*objstore.Source)(nil)

// pinningQueue pins each accepted child's config generation, owned by its
// acceptance, so the generation its freeze recompiles from cannot be pruned
// before the engine binds the child; the bind releases it. It also classifies
// the bound-grant reads every request makes.
type pinningQueue struct {
	custodyQueue
	pins generationPins
}

// ChildAuthority reads the bound grant row; an outage is retryable.
func (q pinningQueue) ChildAuthority(ctx context.Context, parent triggerqueue.ChildParent, occurrence string) (triggerqueue.ChildAuthority, error) {
	bound, err := q.custodyQueue.ChildAuthority(ctx, parent, occurrence)
	return bound, retryableUnlessRefused(err)
}

// CheckChildAuthority checks the presented grant is the bound, live one; an
// outage is retryable, a mismatch is a refusal.
func (q pinningQueue) CheckChildAuthority(ctx context.Context, grant triggerqueue.ChildAuthority, now time.Time) error {
	return retryableUnlessRefused(q.custodyQueue.CheckChildAuthority(ctx, grant, now))
}

func (q pinningQueue) AcceptChild(ctx context.Context, request triggerqueue.ChildAcceptance, now time.Time) (triggerqueue.ChildRecord, bool, error) {
	envelope, err := childworkflow.DecodeStartEnvelope(request.Payload)
	if err != nil {
		return triggerqueue.ChildRecord{}, false, err
	}
	child, duplicate, err := q.custodyQueue.AcceptChild(ctx, request, now)
	if err != nil {
		return child, duplicate, err
	}
	if duplicate {
		// A child the engine already bound has released its pin and needs
		// none: re-pinning would leak it. An unbound duplicate re-pins, which
		// restores a pin lost to an earlier failed response and is otherwise
		// idempotent.
		_, err := q.custodyQueue.ChildEngineStart(ctx, child.Identity)
		if err == nil {
			return child, duplicate, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return triggerqueue.ChildRecord{}, false, err
		}
	}
	if err := q.pins.PinGeneration(ctx, envelope.ConfigGeneration, child.AcceptanceID); err != nil {
		return triggerqueue.ChildRecord{}, false, fmt.Errorf("%w: pin the child's config generation: %w", triggerqueue.ErrChildProposalUnavailable, err)
	}
	return child, duplicate, nil
}
