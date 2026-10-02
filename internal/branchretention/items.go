package branchretention

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instanceannotations"
	"github.com/goobers/goobers/providers"
)

// ItemReader constructs a fresh read-only provider for each retention check.
type ItemReader struct {
	NewProvider       func(string, providers.RepositoryRef) (providers.Provider, error)
	UnknownRepository error
}

// Parked uses no read cache: both discovery and pre-delete checks need current provider
// state. These are bounded, read-only calls and never release or mutate claims.
func (r ItemReader) Parked(ctx context.Context, root, id string, entry instanceannotations.ItemRepository) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	provider, err := r.NewProvider(root, entry.Repository)
	if err != nil {
		return false, err
	}
	if entry.Repository.Provider == providers.ProviderADO {
		// Configured auth can fall back to the sole configured repository.
		// ADO addresses requests using the client's organization, not repo.Owner.
		ado, ok := provider.(*providers.ADOProvider)
		if !ok || entry.Repository.Owner == "" || entry.Repository.Project == "" || !strings.EqualFold(ado.Organization, entry.Repository.Owner) {
			return false, fmt.Errorf("%w: ADO retention destination does not match recorded organization/project", r.UnknownRepository)
		}
	}
	if entry.Kind == "pull_request" {
		item, err := provider.PollPullRequest(ctx, providers.PullRequestPollRequest{Repository: entry.Repository, PullID: id})
		if err != nil {
			return false, err
		}
		if item.State == "" {
			return false, fmt.Errorf("pull request has no current state")
		}
		if entry.Repository.Provider == providers.ProviderADO {
			// ADO's single-PR response omits labels. Absence from that
			// response cannot establish that the PR is not human-parked.
			ado, ok := provider.(*providers.ADOProvider)
			if !ok {
				return false, fmt.Errorf("ADO retention provider cannot read pull request labels")
			}
			item.Labels, err = ado.PullRequestLabelNames(ctx, entry.Repository, id)
		}
		return Parked(item.State, item.Labels), err
	}
	item, err := provider.GetWorkItem(ctx, entry.Repository, id)
	if err == nil && item.State == "" && item.Status == "" {
		return false, fmt.Errorf("item has no current state")
	}
	return Parked(item.State, item.Labels) || Parked(string(item.Status), nil) || item.BlockedByCount > 0, err
}
