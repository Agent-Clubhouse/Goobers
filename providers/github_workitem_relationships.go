package providers

import (
	"context"
	"net/http"
)

// ReadWorkItemRelationships reads one parent and one bounded blocker page. A
// parent 404 is deliberately partial: GitHub documents only resource-not-found,
// which cannot distinguish no parent from inaccessible parent evidence.
func (p *GitHubProvider) ReadWorkItemRelationships(ctx context.Context, repo RepositoryRef, item WorkItem) (WorkItemRelationships, error) {
	var result WorkItemRelationships
	if !githubAttentionOwns(repo, item) || !nativePositiveID(item.ID) || !nativePositiveID(item.StableID) {
		return result, ErrAttentionChanged
	}
	ctx, cancel := attentionContext(ctx)
	defer cancel()
	result.Parents, result.ParentsComplete = p.selectedParent(ctx, repo, item.ID)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	blockers, complete, err := p.attentionBlockers(ctx, repo, item.ID)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, nil
	}
	result.BlockersComplete = complete
	for _, blocker := range blockers {
		target := WorkItemRelationTarget{ID: blocker.ID, StableID: blocker.StableID, Verified: blocker.Verified}
		if blocker.Verified {
			target.URL = "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + blocker.ID
		}
		result.Blockers = append(result.Blockers, target)
	}
	return result, nil
}
func (p *GitHubProvider) selectedParent(ctx context.Context, repo RepositoryRef, id string) ([]WorkItemRelationTarget, bool) {
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "issues", id, "parent")
	if err != nil {
		return nil, false
	}
	var raw githubIssue
	response, err := p.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false
	}
	if err = readAttentionJSON(response, endpoint, '{', &raw); err != nil {
		return nil, false
	}
	parent := mapGitHubIssue(raw)
	verified := githubAttentionOwns(repo, parent) && nativePositiveID(parent.ID) && nativePositiveID(parent.StableID)
	target := WorkItemRelationTarget{ID: parent.ID, StableID: parent.StableID, Verified: verified}
	if verified {
		target.URL = parent.URL
	}
	return []WorkItemRelationTarget{target}, verified
}
