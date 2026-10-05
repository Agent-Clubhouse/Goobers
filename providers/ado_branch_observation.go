package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GetBranch reads one exact ADO branch without mutation. The Refs API filter
// is a prefix, so an adjacent branch must never satisfy an effect receipt.
// https://learn.microsoft.com/en-us/rest/api/azure/devops/git/refs/list?view=azure-devops-rest-7.1
// Four bounded pages cover at most 400 prefix matches; incomplete observation
// returns an error instead of claiming the requested reference is absent.
func (p *ADOProvider) GetBranch(ctx context.Context, repo RepositoryRef, name string) (BranchSummary, bool, error) {
	if err := requireRepo(repo); err != nil {
		return BranchSummary{}, false, err
	}
	name = strings.TrimPrefix(name, "refs/heads/")
	if name == "" || len(name) > 1024 || strings.ContainsAny(name, "\x00\r\n") {
		return BranchSummary{}, false, fmt.Errorf("ado: exact branch name is required")
	}
	base, err := p.repoURL(repo, "refs")
	if err != nil {
		return BranchSummary{}, false, err
	}
	continuation := ""
	seen := map[string]bool{}
	for range 4 {
		query := url.Values{"filter": []string{"heads/" + name}, "$top": []string{"100"}}
		if continuation != "" {
			query.Set("continuationToken", continuation)
		}
		endpoint, err := addQuery(base, query)
		if err != nil {
			return BranchSummary{}, false, err
		}
		refs, next, err := p.branchObservationPage(ctx, endpoint)
		if err != nil {
			return BranchSummary{}, false, err
		}
		for _, ref := range refs.Value {
			if ref.Name == "refs/heads/"+name {
				if ref.ObjectID == "" {
					return BranchSummary{}, false, fmt.Errorf("ado: observed branch has no commit identity")
				}
				return BranchSummary{Name: name, SHA: ref.ObjectID, URL: ref.URL}, true, nil
			}
		}
		if next == "" {
			return BranchSummary{}, false, nil
		}
		if len(next) > 2048 || seen[next] {
			return BranchSummary{}, false, fmt.Errorf("ado: branch observation pagination is invalid")
		}
		seen[next] = true
		continuation = next
	}
	return BranchSummary{}, false, fmt.Errorf("ado: exact branch observation exceeds page bound")
}

func (p *ADOProvider) branchObservationPage(ctx context.Context, endpoint string) (adoRefsResponse, string, error) {
	var refs adoRefsResponse
	response, err := p.send(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return refs, "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return refs, "", fmt.Errorf("ado: branch observation failed with status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return refs, "", err
	}
	if len(data) > 1<<20 {
		return refs, "", fmt.Errorf("ado: branch observation exceeds byte bound")
	}
	if err = json.Unmarshal(data, &refs); err != nil {
		return refs, "", err
	}
	if len(refs.Value) > 100 {
		return refs, "", fmt.Errorf("ado: branch observation exceeds page size")
	}
	return refs, strings.TrimSpace(response.Header.Get("x-ms-continuationtoken")), nil
}
