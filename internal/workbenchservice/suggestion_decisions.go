package workbenchservice

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchsuggestions"
)

// Preview freshly verifies both endpoints and returns the exact derived owner
// diff. No review decision, proposal command or remote effect is recorded.
func (s *SuggestionService) Preview(ctx context.Context, p httpapi.Principal, gaggle string, request workbench.SuggestionPreviewRequest) (workbench.SuggestionPreview, error) {
	var result workbench.SuggestionPreview
	err := s.withSnapshot(ctx, p, gaggle, func(ctx context.Context, snap suggestionSnapshot) error {
		_, bound, err := s.selected(ctx, snap, request.Selection, request.Key)
		if err != nil {
			return err
		}
		verified, err := s.verify(ctx, snap, bound)
		if err != nil {
			return err
		}
		proposer, err := s.Proposals.proposer(ctx, verified.bound, verified.load)
		if err != nil {
			return err
		}
		result.SourceBindingID = verified.bound.read.Source.Spec.Name
		result.Preview, err = proposer.Preview(ctx, verified.request)
		return err
	})
	if err != nil {
		return workbench.SuggestionPreview{}, err
	}
	return result, nil
}

// Decide records one attributed review. Acceptance verifies the preview pins,
// durably links the ordinary proposal before effects, and does not retry an
// already linked proposal. Its separate Continue/Check APIs govern later phases.
func (s *SuggestionService) Decide(ctx context.Context, p httpapi.Principal, gaggle string, request workbench.SuggestionDecisionRequest) (workbench.SuggestionReview, error) {
	if (request.Decision != "accept" && request.Decision != "reject") || len(request.Reason) > 4096 || (request.Decision == "accept") != (request.ExpectedOwner != nil) || (request.Decision == "reject" && request.ExpectedOperationDigest != "") {
		return workbench.SuggestionReview{}, readError(http.StatusBadRequest, "invalid_request", "A bounded review decision and exact acceptance preview are required.")
	}
	var result workbench.SuggestionReview
	err := s.snapshot(ctx, p, gaggle, true, func(ctx context.Context, snap suggestionSnapshot) error {
		loaded, bound, err := s.selected(ctx, snap, request.Selection, request.Key)
		if err != nil {
			return err
		}
		prior, err := s.Proposals.Queue.FindWorkbenchSuggestion(ctx, suggestionScope(p, gaggle), bound.Key)
		if err == nil {
			if err = sameRetainedDecision(prior, request); err != nil {
				return err
			}
			if prior.State != "accepting" {
				result, err = s.reviewView(ctx, snap, prior, true)
				return err
			}
			bound = prior.Input.Suggestion
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		input := suggestionDecisionInput(p, gaggle, loaded, bound, request)
		var verified verifiedSuggestion
		if request.Decision == "accept" {
			verified, err = s.verify(ctx, snap, bound)
			if err != nil {
				return err
			}
			proposal, err := s.acceptanceInput(p, gaggle, verified, request)
			if err != nil {
				return err
			}
			input.Proposal = &proposal
		}
		if prior.ID != "" {
			input = prior.Input
		}
		record, duplicate, err := s.Proposals.Queue.AcceptWorkbenchSuggestion(ctx, input, s.Proposals.now())
		if err != nil {
			return err
		}
		if request.Decision == "reject" {
			result = basicSuggestionView(record, duplicate)
			return nil
		}
		result, err = s.submitDecision(ctx, verified, record, duplicate)
		return err
	})
	return result, err
}

func (s *SuggestionService) acceptanceInput(p httpapi.Principal, gaggle string, verified verifiedSuggestion, request workbench.SuggestionDecisionRequest) (triggerqueue.WorkbenchProposalInput, error) {
	if request.ExpectedOwner == nil || *request.ExpectedOwner != verified.request.Expected {
		return triggerqueue.WorkbenchProposalInput{}, workbench.ErrMetadataRevision
	}
	binding := verified.bound.read.Source.Spec.Name
	target, operation, err := workbench.MetadataOperationDigest(verified.bound.set, binding, verified.request)
	if err != nil {
		return triggerqueue.WorkbenchProposalInput{}, err
	}
	if operation != request.ExpectedOperationDigest {
		return triggerqueue.WorkbenchProposalInput{}, triggerqueue.ErrConflict
	}
	return triggerqueue.WorkbenchProposalInput{Scope: writeScope(p, gaggle, binding), RequestID: triggerqueue.SuggestionProposalKey(request.Key), TargetDigest: target, OperationDigest: operation, Request: verified.request}, nil
}
func (s *SuggestionService) submitDecision(ctx context.Context, verified verifiedSuggestion, record triggerqueue.WorkbenchSuggestion, duplicate bool) (workbench.SuggestionReview, error) {
	proposal, _, err := s.Proposals.Queue.AcceptWorkbenchProposal(ctx, *record.Input.Proposal, s.Proposals.now())
	if err != nil {
		return workbench.SuggestionReview{}, err
	}
	record, err = s.Proposals.Queue.LinkWorkbenchSuggestion(ctx, record.Input.Scope, record.ID, proposal.ID, s.Proposals.now())
	if err != nil {
		return workbench.SuggestionReview{}, err
	}
	proposal, err = s.Proposals.advanceProposal(ctx, verified.bound, verified.load, proposal)
	result := basicSuggestionView(record, duplicate)
	view := proposalView(proposal, duplicate)
	result.Proposal = &view
	return result, err
}

// Review reads current-authorized retained attribution and its linked receipt;
// it performs no provider mutation or phase advancement.
func (s *SuggestionService) Review(ctx context.Context, p httpapi.Principal, gaggle, id string) (workbench.SuggestionReview, error) {
	var result workbench.SuggestionReview
	err := s.withSnapshot(ctx, p, gaggle, func(ctx context.Context, snap suggestionSnapshot) error {
		record, err := s.Proposals.Queue.WorkbenchSuggestion(ctx, suggestionScope(p, gaggle), id)
		if err != nil {
			return err
		}
		result, err = s.reviewView(ctx, snap, record, false)
		return err
	})
	if err != nil {
		return workbench.SuggestionReview{}, err
	}
	return result, nil
}
func (s *SuggestionService) reviewView(ctx context.Context, snap suggestionSnapshot, record triggerqueue.WorkbenchSuggestion, duplicate bool) (workbench.SuggestionReview, error) {
	if err := snap.authorizeSuggestion(record.Input.Suggestion); err != nil {
		return workbench.SuggestionReview{}, err
	}
	result := basicSuggestionView(record, duplicate)
	if record.ProposalID == "" {
		return result, nil
	}
	input := record.Input.Proposal
	selected, _, err := snap.readAccess(input.Scope.SourceBindingID)
	if err != nil {
		return workbench.SuggestionReview{}, err
	}
	bound := proposalBinding{set: snap.set, read: selected}
	proposal, err := s.Proposals.Queue.WorkbenchProposal(ctx, input.Scope, record.ProposalID)
	if err != nil {
		return workbench.SuggestionReview{}, err
	}
	if !proposalTargetConfigured(bound, proposal) {
		return workbench.SuggestionReview{}, workbench.ErrSuggestion
	}
	view := proposalView(proposal, duplicate)
	result.Proposal = &view
	return result, nil
}
func basicSuggestionView(record triggerqueue.WorkbenchSuggestion, duplicate bool) workbench.SuggestionReview {
	return workbench.SuggestionReview{ID: record.ID, Suggestion: record.Input.Suggestion, State: record.State, Decision: record.Input.Decision, Reason: record.Input.Reason, AcceptedAt: record.AcceptedAt, Duplicate: duplicate}
}
func sameRetainedDecision(prior triggerqueue.WorkbenchSuggestion, request workbench.SuggestionDecisionRequest) error {
	if prior.Input.Decision != request.Decision || prior.Input.Reason != request.Reason {
		return triggerqueue.ErrConflict
	}
	if request.Decision == "accept" && (request.ExpectedOwner == nil || prior.Input.Proposal.Request.Expected != *request.ExpectedOwner || prior.Input.Proposal.OperationDigest != request.ExpectedOperationDigest) {
		return triggerqueue.ErrConflict
	}
	return nil
}
func suggestionDecisionInput(p httpapi.Principal, gaggle string, loaded workbenchsuggestions.Loaded, bound workbench.BoundSuggestion, request workbench.SuggestionDecisionRequest) triggerqueue.WorkbenchSuggestionInput {
	return triggerqueue.WorkbenchSuggestionInput{Scope: suggestionScope(p, gaggle), Suggestion: bound, ConfigGeneration: loaded.ConfigGeneration, ArtifactSequence: loaded.Artifact.Sequence, StageSequence: loaded.Artifact.StageSequence, Branch: loaded.Artifact.Branch, Decision: request.Decision, Reason: request.Reason}
}
func (s *SuggestionService) selected(ctx context.Context, snap suggestionSnapshot, selection workbench.SuggestionSelection, key string) (workbenchsuggestions.Loaded, workbench.BoundSuggestion, error) {
	loaded, err := s.loadArtifact(ctx, snap, selection)
	if err != nil {
		return loaded, workbench.BoundSuggestion{}, err
	}
	for _, bound := range loaded.Suggestions {
		if bound.Key == key {
			return loaded, bound, snap.authorizeSuggestion(bound)
		}
	}
	return loaded, workbench.BoundSuggestion{}, workbench.ErrSuggestion
}
