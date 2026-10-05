package workbenchservice

import (
	"context"
	"net/http"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
)

// RepositoryProposalFactory receives only the callback-scoped human credential.
type RepositoryProposalFactory func(context.Context, ReadBinding, interactiveaccess.Credential) (workbenchprovider.RepositoryProposalClient, error)

// ProposalService combines current repository policy with durable one-attempt
// phase custody. Provider effects must finish inside the held policy callback.
type ProposalService struct {
	ReadService *Service
	Queue       *triggerqueue.Store
	Provider    RepositoryProposalFactory
	Now         func() time.Time
}
type proposalBinding struct {
	set  workbench.SourceSet
	read ReadBinding
}

// Preview returns a current-source diff and performs no remote mutation.
func (s *ProposalService) Preview(ctx context.Context, p httpapi.Principal, gaggle, binding string, request workbench.MetadataChangeRequest) (workbench.MetadataPreview, error) {
	request = copyMetadataRequest(request)
	var preview workbench.MetadataPreview
	err := s.withProposal(ctx, p, gaggle, binding, "source.proposeChange", func(ctx context.Context, bound proposalBinding, load interactiveaccess.RepositoryCredentialLoader) error {
		if _, _, err := workbench.MetadataOperationDigest(bound.set, binding, request); err != nil {
			return err
		}
		proposer, err := s.proposer(ctx, bound, load)
		if err != nil {
			return err
		}
		preview, err = proposer.Preview(ctx, request)
		if err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		return workbench.MetadataPreview{}, proposalError(err)
	}
	return preview, nil
}

// Submit explicitly advances acknowledged or separately observed phases. Each
// native call is claimed durably first. Unknown phases never replay on retries.
func (s *ProposalService) Submit(ctx context.Context, p httpapi.Principal, gaggle, binding, key string, request workbench.MetadataChangeRequest) (workbench.MetadataProposalCommand, error) {
	request = copyMetadataRequest(request)
	var result workbench.MetadataProposalCommand
	err := s.withProposal(ctx, p, gaggle, binding, "source.proposeChange", func(ctx context.Context, bound proposalBinding, load interactiveaccess.RepositoryCredentialLoader) error {
		target, operation, err := workbench.MetadataOperationDigest(bound.set, binding, request)
		if err != nil {
			return err
		}
		input := triggerqueue.WorkbenchProposalInput{Scope: writeScope(p, gaggle, binding), RequestID: key, TargetDigest: target, OperationDigest: operation, Request: request}
		record, duplicate, err := s.Queue.AcceptWorkbenchProposal(ctx, input, s.now())
		if err != nil {
			return err
		}
		record, err = s.advanceProposal(ctx, bound, load, record)
		result = proposalView(record, duplicate)
		return err
	})
	return result, proposalError(err)
}

// Command reads current-actor custody without provider I/O. Read authorization
// does not restore a revoked field allowlist or authorize another source effect.
func (s *ProposalService) Command(ctx context.Context, p httpapi.Principal, gaggle, binding, id string) (workbench.MetadataProposalCommand, error) {
	return s.inspect(ctx, p, gaggle, binding, id, false)
}

// Check performs one bounded read observation of the exact last claimed phase.
// It never advances to a new phase or repeats any provider mutation.
func (s *ProposalService) Check(ctx context.Context, p httpapi.Principal, gaggle, binding, id string) (workbench.MetadataProposalCommand, error) {
	return s.inspect(ctx, p, gaggle, binding, id, true)
}

func (s *ProposalService) withProposal(ctx context.Context, p httpapi.Principal, gaggle, binding string, action apiv1.InteractiveAction, use func(context.Context, proposalBinding, interactiveaccess.RepositoryCredentialLoader) error) error {
	if s == nil || s.ReadService == nil || s.ReadService.Permissions == nil || s.Queue == nil || s.Provider == nil {
		return readError(http.StatusServiceUnavailable, "workbench_proposals_unavailable", "Repository metadata proposals are unavailable.")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var selected proposalBinding
	return s.ReadService.Permissions.WithRepositorySource(ctx, p, gaggle, action, func(g *apiv1.Gaggle) (interactiveaccess.Target, error) {
		set, err := workbench.BindSources(*g)
		if err != nil {
			return interactiveaccess.Target{}, readError(http.StatusConflict, "workbench_invalid_sources", "Workbench sources are not valid in the applied configuration.")
		}
		for _, source := range set.Sources {
			if source.Spec.Name == binding && (source.Spec.Kind == "documents" || source.Spec.Kind == "relationships") {
				selected = proposalBinding{set: set, read: ReadBinding{Scope: set.Scope, Source: source, Generation: configDigest(g)}}
				return interactiveaccess.Target{Kind: "repository", Repository: *source.Spec.Repository}, nil
			}
		}
		return interactiveaccess.Target{}, readError(http.StatusNotFound, "workbench_source_not_found", "No repository source with this binding is configured in the gaggle.")
	}, func(ctx context.Context, load interactiveaccess.RepositoryCredentialLoader) error {
		return use(ctx, selected, load)
	})
}
func (s *ProposalService) proposer(ctx context.Context, bound proposalBinding, load interactiveaccess.RepositoryCredentialLoader) (*workbenchprovider.RepositoryProposer, error) {
	credential, err := load(ctx)
	if err != nil {
		return nil, err
	}
	client, err := s.Provider(ctx, bound.read, credential)
	if err != nil {
		return nil, err
	}
	return workbenchprovider.NewRepositoryProposer(bound.set, bound.read.Source.Spec.Name, client)
}
func (s *ProposalService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func copyMetadataRequest(r workbench.MetadataChangeRequest) workbench.MetadataChangeRequest {
	if r.Objective != nil {
		value := *r.Objective
		r.Objective = &value
	}
	if r.Alias != nil {
		value := *r.Alias
		r.Alias = &value
	}
	if r.Value != nil {
		value := *r.Value
		r.Value = &value
	}
	if r.Relationship != nil {
		edge := *r.Relationship
		r.Relationship = &edge
	}
	return r
}

// Continue is an explicit new command to advance the existing immutable intent.
// It is useful after a read-only Check or after a bounded request stopped between
// phases; no client-supplied content, pins or destination can replace that intent.
func (s *ProposalService) Continue(ctx context.Context, p httpapi.Principal, gaggle, binding, id string) (workbench.MetadataProposalCommand, error) {
	var result workbench.MetadataProposalCommand
	err := s.withProposal(ctx, p, gaggle, binding, "source.proposeChange", func(ctx context.Context, bound proposalBinding, load interactiveaccess.RepositoryCredentialLoader) error {
		record, err := s.Queue.WorkbenchProposal(ctx, writeScope(p, gaggle, binding), id)
		if err != nil {
			return err
		}
		target, operation, err := workbench.MetadataOperationDigest(bound.set, binding, record.Input.Request)
		if err != nil {
			return err
		}
		if target != record.Input.TargetDigest || operation != record.Input.OperationDigest {
			return interactiveaccess.ErrDenied
		}
		record, err = s.advanceProposal(ctx, bound, load, record)
		result = proposalView(record, true)
		return err
	})
	return result, proposalError(err)
}
