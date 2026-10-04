package workbenchservice

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
)

func (s *ProposalService) advanceProposal(ctx context.Context, bound proposalBinding, load interactiveaccess.RepositoryCredentialLoader, record triggerqueue.WorkbenchProposal) (triggerqueue.WorkbenchProposal, error) {
	if record.State != "accepted" && record.State != "prepared" {
		return record, nil
	}
	proposer, err := s.proposer(ctx, bound, load)
	if err != nil {
		return s.stopProposal(ctx, record)
	}
	if record.State == "accepted" {
		record, err = s.prepareProposal(ctx, bound, proposer, record)
		if err != nil || record.State != "prepared" {
			return record, err
		}
	}
	for record.State == "prepared" {
		if ctx.Err() != nil {
			return record, nil
		}
		index := len(record.Phases)
		input, err := triggerqueue.WorkbenchProposalPhaseInput(record, index)
		if err != nil {
			return record, err
		}
		var claimed bool
		record, claimed, err = s.Queue.ClaimWorkbenchProposalPhase(ctx, record.Input.Scope, record.ID, record.RequestDigest, input.Phase, s.now())
		if err != nil || !claimed {
			return record, err
		}
		// Native errors do not erase whether a POST began. The result preserves the
		// uncertain boundary, with no provider error text copied into public custody.
		result, _ := proposer.Apply(ctx, record.Input.Request, record.Plan.Preview, input)
		receiptctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		record, err = s.Queue.CompleteWorkbenchProposalPhase(receiptctx, record.Input.Scope, record.ID, record.RequestDigest, index, result, s.now())
		cancel()
		if err != nil {
			return record, err
		}
	}
	return record, nil
}
func (s *ProposalService) prepareProposal(ctx context.Context, bound proposalBinding, proposer *workbenchprovider.RepositoryProposer, record triggerqueue.WorkbenchProposal) (triggerqueue.WorkbenchProposal, error) {
	preview, err := proposer.Preview(ctx, record.Input.Request)
	if err != nil || !preview.Changed {
		return s.stopProposal(ctx, record)
	}
	native, err := proposer.Prepare(record.ID[10:], record.Input.Request, preview)
	if err != nil {
		return s.stopProposal(ctx, record)
	}
	plan := triggerqueue.WorkbenchProposalPlan{Scope: bound.set.Scope, Kind: bound.read.Source.Spec.Kind, Preview: preview, Native: native}
	return s.Queue.AttachWorkbenchProposalPlan(ctx, record.Input.Scope, record.ID, record.RequestDigest, plan)
}
func (s *ProposalService) stopProposal(ctx context.Context, record triggerqueue.WorkbenchProposal) (triggerqueue.WorkbenchProposal, error) {
	receiptctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return s.Queue.StopWorkbenchProposal(receiptctx, record.Input.Scope, record.ID, record.RequestDigest, s.now())
}
func (s *ProposalService) inspect(ctx context.Context, p httpapi.Principal, gaggle, binding, id string, check bool) (workbench.MetadataProposalCommand, error) {
	var result workbench.MetadataProposalCommand
	err := s.withProposal(ctx, p, gaggle, binding, "repository.read", func(ctx context.Context, bound proposalBinding, load interactiveaccess.RepositoryCredentialLoader) error {
		record, err := s.Queue.WorkbenchProposal(ctx, writeScope(p, gaggle, binding), id)
		if err != nil && !errors.Is(err, triggerqueue.ErrWorkbenchCommandExpired) {
			return err
		}
		if !proposalTargetConfigured(bound, record) {
			return interactiveaccess.ErrDenied
		}
		if err != nil {
			return err
		}
		if check && (record.State == "attempting" || record.State == "unknown") {
			record, err = s.observeProposal(ctx, bound, load, record)
			if err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		result = proposalView(record, false)
		return nil
	})
	if err != nil {
		return workbench.MetadataProposalCommand{}, proposalError(err)
	}
	return result, nil
}
func (s *ProposalService) observeProposal(ctx context.Context, bound proposalBinding, load interactiveaccess.RepositoryCredentialLoader, record triggerqueue.WorkbenchProposal) (triggerqueue.WorkbenchProposal, error) {
	proposer, err := s.proposer(ctx, bound, load)
	if err != nil {
		return record, err
	}
	index := len(record.Phases) - 1
	input, err := triggerqueue.WorkbenchProposalPhaseInput(record, index)
	if err != nil {
		return record, err
	}
	observed, err := proposer.Observe(ctx, input)
	if err != nil {
		return record, err
	}
	return s.Queue.ObserveWorkbenchProposal(ctx, record.Input.Scope, record.ID, record.RequestDigest, index, observed, s.now())
}

// Tombstones retain only digests. Check the bounded declared path set to prove
// exact current target before returning even a generic expired receipt.
func proposalTargetConfigured(bound proposalBinding, record triggerqueue.WorkbenchProposal) bool {
	for _, path := range bound.read.Source.Spec.Paths {
		if record.Input.Request.Path != "" && record.Input.Request.Path != path {
			continue
		}
		target, err := workbench.MetadataTargetDigest(bound.set, bound.read.Source.Spec.Name, path)
		if err == nil && target == record.Input.TargetDigest {
			return true
		}
	}
	return false
}
