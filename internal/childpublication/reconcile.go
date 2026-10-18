package childpublication

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

// admittedPublicationObserver exposes only reads on the exact currently admitted target.
// Recovery cannot change the branch or replay OpenPullRequest on an unknown reply.
type admittedPublicationObserver struct {
	publisher Publisher
	target    Target
}

func (o admittedPublicationObserver) GetBranch(ctx context.Context, repo providers.RepositoryRef, head string) (providers.BranchSummary, bool, error) {
	if repo != o.target.Repository || head != o.target.Head {
		return providers.BranchSummary{}, false, triggerqueue.ErrConflict
	}
	sha, err := o.publisher.Git.Head(ctx, o.target.Workspace, o.target.Remote, head)
	return providers.BranchSummary{Name: head, SHA: sha}, sha != "", err
}
func (o admittedPublicationObserver) FindPullRequestByBranch(ctx context.Context, repo providers.RepositoryRef, head, base string) (providers.PullRequestResult, bool, error) {
	if repo != o.target.Repository || head != o.target.Head || base != o.target.Base {
		return providers.PullRequestResult{}, false, triggerqueue.ErrConflict
	}
	return o.publisher.PRs.FindPullRequestByBranch(ctx, repo, head, base)
}
func (p Publisher) reconcilePR(ctx context.Context, t Target, intent triggerqueue.ChildPublication) (providers.PullRequestResult, error) {
	var result providers.PullRequestResult
	status, err := (Reconciler{Queue: p.Queue, Observer: admittedPublicationObserver{publisher: p, target: t}}).Check(ctx, t.Child.Identity, ActionPR, intent.Digest)
	if err != nil {
		return result, err
	}
	if status.State != "confirmed" {
		return result, errors.New("child PR outcome remains uncertain; no duplicate creation is permitted")
	}
	confirmed, err := p.Queue.ChildPublication(ctx, t.Child.Identity, "pr")
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(confirmed.Receipt, &result)
	return result, err
}
