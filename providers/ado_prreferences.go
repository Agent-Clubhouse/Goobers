package providers

import (
	"context"
	"net/http"
	"strings"
)

// PullRequestReferences is what an Azure DevOps pull request says about the
// work items it implements: its full description and the ids of the work
// items natively linked to it.
type PullRequestReferences struct {
	Body        string
	WorkItemIDs []string
}

// PullRequestReferences reads pullID's full description and its native
// work-item links. The pull-request list response carries neither (its
// description is truncated), so callers that must see every work item an open
// pull request speaks for, such as backlog-query's open-PR eligibility
// backstop, read them here per pull request.
func (p *ADOProvider) PullRequestReferences(ctx context.Context, repo RepositoryRef, pullID string) (PullRequestReferences, error) {
	if err := requireRepo(repo); err != nil {
		return PullRequestReferences{}, err
	}
	detail, err := p.getPullRequestDetail(ctx, repo, pullID)
	if err != nil {
		return PullRequestReferences{}, err
	}
	endpoint, err := p.repoURL(repo, "pullrequests", pullID, "workitems")
	if err != nil {
		return PullRequestReferences{}, err
	}
	var links struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &links); err != nil {
		return PullRequestReferences{}, err
	}
	out := PullRequestReferences{Body: detail.Description}
	for _, link := range links.Value {
		if id := strings.TrimSpace(link.ID); id != "" {
			out.WorkItemIDs = append(out.WorkItemIDs, id)
		}
	}
	return out, nil
}
