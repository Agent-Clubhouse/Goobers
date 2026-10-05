package interactiveaccess

import (
	"context"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

// ProposalSnapshotAccess checks one target/action without minting credentials.
// Its returned loader and all provider work must finish inside the callback.
type ProposalSnapshotAccess func(apiv1.InteractiveAction, Target) (RepositoryCredentialLoader, error)

// WithSourceProposalSnapshot holds one applied source policy while a human
// inspects related sources and explicitly submits a reviewed metadata proposal.
// Every source requires an independent read action; the destination separately
// requires source.proposeChange. Callers may not nest policy operations.
func (s *Service) WithSourceProposalSnapshot(ctx context.Context, p httpapi.Principal, gaggle string, use func(context.Context, *apiv1.Gaggle, ProposalSnapshotAccess) error) error {
	return s.withSourceProposalSnapshot(ctx, p, gaggle, false, use)
}

// WithSourceReviewDecision additionally requires the current human operator's
// source.proposeChange grant even when the decision only rejects a suggestion.
// Viewing source data alone never permits mutation of operational review state.
func (s *Service) WithSourceReviewDecision(ctx context.Context, p httpapi.Principal, gaggle string, use func(context.Context, *apiv1.Gaggle, ProposalSnapshotAccess) error) error {
	return s.withSourceProposalSnapshot(ctx, p, gaggle, true, use)
}

func (s *Service) withSourceProposalSnapshot(ctx context.Context, p httpapi.Principal, gaggle string, decision bool, use func(context.Context, *apiv1.Gaggle, ProposalSnapshotAccess) error) error {
	if use == nil || !human(p) || p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 {
		return ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.lockSourceSnapshot(ctx); err != nil {
		return err
	}
	defer s.mu.RUnlock()
	g := s.gaggles[gaggle]
	if g == nil {
		return ErrDenied
	}
	view, _ := membership(p, g.Spec.InteractiveAccess)
	if !view {
		return ErrDenied
	}
	if decision {
		if err := authorize(p, g, "source.proposeChange"); err != nil {
			return err
		}
	}
	access := func(action apiv1.InteractiveAction, target Target) (RepositoryCredentialLoader, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := proposalSnapshotAction(p, g, action, target); err != nil {
			return nil, err
		}
		if _, err := selectSource(g, s.sources, target); err != nil {
			return nil, err
		}
		return func(callctx context.Context) (Credential, error) {
			if err := ctx.Err(); err != nil {
				return Credential{}, err
			}
			return s.restartCredential(callctx, p, g, action, target)
		}, nil
	}
	if err := use(ctx, g.DeepCopy(), access); err != nil {
		return err
	}
	return ctx.Err()
}

func proposalSnapshotAction(p httpapi.Principal, g *apiv1.Gaggle, action apiv1.InteractiveAction, target Target) error {
	if !actionTarget(action, target) {
		return ErrDenied
	}
	if !readAction(action) && action != "source.proposeChange" {
		return ErrDenied
	}
	if action == "source.proposeChange" && (target.Kind != "repository" || (g.Spec.InteractiveAccess.SourceWrites != nil && g.Spec.InteractiveAccess.SourceWrites.Mode != "pull-request")) {
		return ErrDenied
	}
	return authorize(p, g, action)
}
