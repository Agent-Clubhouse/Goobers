package remediationselect

import (
	"context"

	"github.com/goobers/goobers/providers"
)

// Provider is the forge surface the remediation selection reads. cmd/goobers's
// wider remediationProvider satisfies it; the cloud counter supplies its own.
type Provider interface {
	ListComments(ctx context.Context, repo providers.RepositoryRef, id string) ([]providers.Comment, error)
	GetWorkItem(ctx context.Context, repo providers.RepositoryRef, id string) (providers.WorkItem, error)
	GetPullRequest(ctx context.Context, repo providers.RepositoryRef, pullID string) (providers.PullRequestSummary, error)
	BranchTipSHA(ctx context.Context, repo providers.RepositoryRef, branch string) (string, error)
	CompareCommits(ctx context.Context, repo providers.RepositoryRef, base, head string) (providers.CompareResult, error)
}
