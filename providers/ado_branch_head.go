package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ReadBranchHead observes one exact ADO branch ref. The refs API filter is a
// prefix filter; a sibling such as repair-old is not evidence for repair.
// This does not claim the broader branch-reconciliation/activity capability.
func (p *ADOProvider) ReadBranchHead(ctx context.Context, repo RepositoryRef, branch string) (string, bool, error) {
	name := strings.TrimPrefix(branch, "refs/heads/")
	if name == "" {
		return "", false, fmt.Errorf("branch name is required")
	}
	endpoint, err := p.repoURL(repo, "refs")
	if err != nil {
		return "", false, err
	}
	endpoint, err = addQuery(endpoint, url.Values{"filter": []string{"heads/" + name}})
	if err != nil {
		return "", false, err
	}
	var response adoRefsResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return "", false, err
	}
	var sha string
	for _, ref := range response.Value {
		if ref.Name != "refs/heads/"+name {
			continue
		}
		if sha != "" || ref.ObjectID == "" {
			return "", false, fmt.Errorf("ADO returned an ambiguous or empty exact branch ref")
		}
		sha = ref.ObjectID
	}
	return sha, sha != "", nil
}
