package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

// Read-only interface prevents admission from turning into provider mutation.
// The client and its delivered credential live only in the bounded policy lease.
type restartReadProvider interface {
	GetBranch(context.Context, providers.RepositoryRef, string) (providers.BranchSummary, bool, error)
	GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error)
	PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error)
}

func restartProvider(repo apiv1.RepoRef, credential interactiveaccess.Credential) (restartReadProvider, error) {
	if credential.Value == "" || (!credential.ExpiresAt.IsZero() && !credential.ExpiresAt.After(time.Now())) {
		return nil, interactiveaccess.ErrCredentialUnavailable
	}
	switch repo.Provider {
	case apiv1.ProviderGitHub:
		if credential.Scheme != "bearer" || (repo.BaseURL != "" && repo.BaseURL != "https://github.com") {
			return nil, interactiveaccess.ErrCredentialUnavailable
		}
		return providers.NewGitHubProvider(credential.Value), nil
	case apiv1.ProviderADO:
		if repo.BaseURL != "" && repo.BaseURL != "https://dev.azure.com" {
			return nil, interactiveaccess.ErrCredentialUnavailable
		}
		kind := providers.ADOCredentialKindBearer
		if credential.Scheme == "basic" {
			kind = providers.ADOCredentialKindPAT
		} else if credential.Scheme != "bearer" {
			return nil, interactiveaccess.ErrCredentialUnavailable
		}
		source, err := providers.NewADODeliveredCredentialSourceWithExpiry(kind, credential.Value, "interactive restart read", credential.ExpiresAt)
		if err != nil {
			return nil, err
		}
		return restartADOReader{providers.NewADOProvider(repo.Owner, repo.Project, "", providers.WithADOCredentialSource(source))}, nil
	default:
		return nil, interactiveaccess.ErrCredentialUnavailable
	}
}

type restartADOReader struct{ *providers.ADOProvider }

func (p restartADOReader) GetBranch(ctx context.Context, repo providers.RepositoryRef, name string) (providers.BranchSummary, bool, error) {
	sha, found, err := p.ReadBranchHead(ctx, repo, name)
	return providers.BranchSummary{Name: name, SHA: sha}, found, err
}

func verifyRestartClaims(ctx context.Context, source journal.RunIdentity, claims []localscheduler.ClaimEntry, code restartReadProvider, codeRepo providers.RepositoryRef, backlog restartReadProvider, backlogRepo providers.RepositoryRef, policy *continuationEligibilityPolicy) error {
	if len(claims) > 512 {
		return errors.New("restart source claims exceed admission limit")
	}
	for _, claim := range claims {
		if claim.Gaggle != source.Gaggle || claim.RunID != source.RunID || claim.Workflow != source.Workflow || claim.Provider == "" || claim.ExternalID == "" {
			return restartRefusal("restart_claim_identity", "A source claim has missing or foreign ownership metadata.")
		}
		if !claim.SharedDeadline.IsZero() || claim.SharedOwner != (sharedclaim.Owner{}) || claim.SharedRevoked {
			return restartRefusal("restart_shared_claim", "Restart of shared provider claims requires a shared-lease transfer.")
		}
		id := continuationClaimID(claim)
		if strings.HasPrefix(id, pullRequestClaimPrefix) {
			if code == nil || claim.Provider != string(codeRepo.Provider) {
				return restartRefusal("restart_claim_provider", "The pull request claim does not match its configured code provider.")
			}
			poll, err := code.PollPullRequest(ctx, providers.PullRequestPollRequest{Repository: codeRepo, PullID: strings.TrimPrefix(id, pullRequestClaimPrefix)})
			if err != nil {
				return err
			}
			if !strings.EqualFold(poll.State, "open") {
				return fmt.Errorf("restart source pull request %q is not open", id)
			}
			continue
		}
		if backlog == nil || claim.Provider != string(backlogRepo.Provider) {
			return restartRefusal("restart_claim_provider", "The item claim does not match its configured backlog provider.")
		}
		item, err := backlog.GetWorkItem(ctx, backlogRepo, id)
		if err != nil {
			return err
		}
		if !strings.EqualFold(item.State, "open") {
			return fmt.Errorf("restart source item %q is not open", id)
		}
		if err := validateContinuationEligibility(item, backlogRepo.Provider, id, policy); err != nil {
			return err
		}
	}
	return nil
}
