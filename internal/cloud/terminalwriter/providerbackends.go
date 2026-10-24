package terminalwriter

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/providers"
)

// restProvider is the GitHub and Gitea surface a Backend needs: both model a
// pull request as an issue for labels.
type restProvider interface {
	Kind() providers.ProviderKind
	GetWorkItem(ctx context.Context, repo providers.RepositoryRef, id string) (providers.WorkItem, error)
	ListComments(ctx context.Context, repo providers.RepositoryRef, id string) ([]providers.Comment, error)
	UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error)
	UpdateComment(ctx context.Context, repo providers.RepositoryRef, commentID, body string) error
	DeleteBranch(ctx context.Context, req providers.DeleteBranchRequest) (providers.DeleteBranchResult, error)
	ReleaseWorkItemClaim(ctx context.Context, req providers.ClaimWorkItemRequest) (providers.WorkItem, error)
}

type restBackend struct{ restProvider }

func githubBackend(p *providers.GitHubProvider) Backend { return restBackend{p} }
func giteaBackend(p *providers.GiteaProvider) Backend   { return giteaRESTBackend{restBackend{p}} }

// giteaRESTBackend refuses branch deletion: Gitea deletes a branch with a git
// push, and the control role has neither a command runner nor git. Without
// this the delete would fail as a retryable error on every attempt.
type giteaRESTBackend struct{ restBackend }

func (giteaRESTBackend) DeleteBranch(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
	return providers.DeleteBranchResult{}, fmt.Errorf("gitea branch delete needs git in the control role: %w", ErrUnsupported)
}

func (b restBackend) PullLabels(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]string, error) {
	item, err := b.GetWorkItem(ctx, repo, pullID)
	return item.Labels, err
}

func (b restBackend) LabelPull(ctx context.Context, repo providers.RepositoryRef, pullID string, add, remove []string) error {
	_, err := b.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{Repository: repo, ID: pullID, AddLabels: add, RemoveLabels: remove})
	return err
}

// adoBackend is Azure DevOps: work items are Azure Boards, pull request labels
// are Azure Repos, and a branch an open pull request uses is kept.
type adoBackend struct{ *providers.ADOProvider }

func (b adoBackend) UpdateComment(context.Context, providers.RepositoryRef, string, string) error {
	return fmt.Errorf("azure boards comment update: %w", ErrUnsupported)
}

func (b adoBackend) PullLabels(ctx context.Context, repo providers.RepositoryRef, pullID string) ([]string, error) {
	return b.PullRequestLabelNames(ctx, repo, pullID)
}

func (b adoBackend) LabelPull(ctx context.Context, repo providers.RepositoryRef, pullID string, add, remove []string) error {
	if len(add) > 0 {
		if err := b.AddPullRequestLabels(ctx, repo, pullID, add); err != nil {
			return err
		}
	}
	for _, name := range remove {
		if err := b.RemovePullRequestLabel(ctx, repo, pullID, name); err != nil {
			return err
		}
	}
	return nil
}

func (b adoBackend) DeleteBranch(ctx context.Context, req providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
	return b.DeleteUnusedBranch(ctx, req)
}
